package audio

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestRingHoldsBackUntilPrefilled(t *testing.T) {
	ring := NewRing(1000, 100, 1)
	if _, err := ring.Write(bytes.Repeat([]byte{1}, 50)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := make([]byte, 200)
	n, eof := ring.ReadAvailable(out)
	if n != 0 || eof {
		t.Fatalf("expected the prefill gate to hold playback, got n=%d eof=%v", n, eof)
	}

	if _, err := ring.Write(bytes.Repeat([]byte{2}, 60)); err != nil {
		t.Fatalf("write: %v", err)
	}
	n, eof = ring.ReadAvailable(out)
	if n != 110 || eof {
		t.Fatalf("expected 110 bytes once prefilled, got n=%d eof=%v", n, eof)
	}
}

// A clip shorter than the prefill target must still play — otherwise a short
// bumper or station ident would sit in the buffer and never be heard.
func TestRingReleasesShortSourceOnWriterClose(t *testing.T) {
	ring := NewRing(1000, 500, 1)
	if _, err := ring.Write([]byte("short")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 64)
	if n, _ := ring.ReadAvailable(out); n != 0 {
		t.Fatalf("expected the gate to hold before close, got %d", n)
	}

	ring.CloseWrite()
	n, eof := ring.ReadAvailable(out)
	if n != 5 || !eof {
		t.Fatalf("expected the whole short clip and EOF, got n=%d eof=%v", n, eof)
	}
}

// EOF must not be reported until buffered audio has drained, or the tail of
// every track would be cut off as the player advanced.
func TestRingDefersEOFUntilDrained(t *testing.T) {
	ring := NewRing(1000, 0, 1)
	if _, err := ring.Write(bytes.Repeat([]byte{7}, 10)); err != nil {
		t.Fatalf("write: %v", err)
	}
	ring.CloseWrite()

	out := make([]byte, 4)
	if n, eof := ring.ReadAvailable(out); n != 4 || eof {
		t.Fatalf("expected buffered data without EOF, got n=%d eof=%v", n, eof)
	}
	if n, eof := ring.ReadAvailable(out); n != 4 || eof {
		t.Fatalf("expected buffered data without EOF, got n=%d eof=%v", n, eof)
	}
	if n, eof := ring.ReadAvailable(out); n != 2 || !eof {
		t.Fatalf("expected the last bytes with EOF, got n=%d eof=%v", n, eof)
	}
}

// Reads never block: an empty ring is a silent frame, not a stalled sound card.
func TestRingReadDoesNotBlockWhenEmpty(t *testing.T) {
	ring := NewRing(1000, 0, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out := make([]byte, 32)
		ring.ReadAvailable(out)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReadAvailable blocked on an empty ring")
	}
}

// Writes block when full, which is the backpressure that stops ffmpeg decoding
// an entire album into memory ahead of playback.
func TestRingWriteBlocksUntilDrained(t *testing.T) {
	ring := NewRing(64, 0, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	written := make(chan int, 1)
	go func() {
		defer wg.Done()
		n, err := ring.Write(bytes.Repeat([]byte{9}, 96))
		if err != nil {
			t.Errorf("write: %v", err)
		}
		written <- n
	}()

	select {
	case <-written:
		t.Fatal("write returned before the ring had room")
	case <-time.After(50 * time.Millisecond):
	}

	out := make([]byte, 64)
	ring.ReadAvailable(out)
	select {
	case n := <-written:
		if n != 96 {
			t.Fatalf("expected 96 bytes written, got %d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("write never completed after the ring drained")
	}
	wg.Wait()
}

func TestRingWrapsAround(t *testing.T) {
	ring := NewRing(8, 0, 1)
	if _, err := ring.Write([]byte("abcdef")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 4)
	ring.ReadAvailable(out)
	if string(out) != "abcd" {
		t.Fatalf("expected abcd, got %q", out)
	}
	// This write straddles the end of the backing array.
	if _, err := ring.Write([]byte("ghijk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	rest := make([]byte, 7)
	n, _ := ring.ReadAvailable(rest)
	if string(rest[:n]) != "efghijk" {
		t.Fatalf("expected efghijk, got %q", rest[:n])
	}
}

// Closing a ring wakes a blocked writer, so killing a decode never leaks the
// goroutine feeding it.
func TestRingCloseUnblocksWriter(t *testing.T) {
	ring := NewRing(16, 0, 1)
	done := make(chan error, 1)
	go func() {
		_, err := ring.Write(bytes.Repeat([]byte{1}, 64))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ring.Close()
	select {
	case err := <-done:
		if err != ErrRingClosed {
			t.Fatalf("expected ErrRingClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing the ring did not unblock the writer")
	}
}

func TestFormatConversions(t *testing.T) {
	format := DefaultFormat
	if got := format.BytesPerSecond(); got != 48000*4 {
		t.Fatalf("bytes per second: got %d", got)
	}
	// A frame must never be a fraction of a sample or the channel interleave
	// would drift.
	if got := format.BytesForDuration(0.01) % format.BytesPerFrame(); got != 0 {
		t.Fatalf("frame size is not sample-aligned: remainder %d", got)
	}
	if got := format.SecondsForBytes(int64(format.BytesPerSecond())); got != 1 {
		t.Fatalf("seconds for one second of bytes: got %v", got)
	}
}

// An underrun must not byte-shift everything that follows.
//
// The writer is a pipe, so it hands over whatever byte count the kernel felt
// like. While the ring is full that is invisible, because the read is capped at
// the caller's frame-sized buffer. The moment it runs dry the read returns a
// partial frame and moves the cursor by that amount — and on 16-bit stereo an
// odd remainder byte-swaps every sample from then on, which is full-scale
// white noise that lasts until the item ends.
func TestAnUnderrunDoesNotMisalignTheStream(t *testing.T) {
	const frame = 4 // 16-bit stereo
	ring := NewRing(4096, 0, frame)

	// Seven bytes: one whole frame and three bytes of the next.
	if _, err := ring.Write([]byte{1, 2, 3, 4, 5, 6, 7}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 2*frame)
	n, eof := ring.ReadAvailable(buf)
	if eof {
		t.Fatal("the writer has not finished, so this is not EOF")
	}
	if n%frame != 0 {
		t.Fatalf("read %d bytes, which is not a whole number of %d-byte frames", n, frame)
	}
	if n != frame {
		t.Fatalf("expected the one whole frame, got %d bytes", n)
	}

	// The three orphaned bytes must still be at the front of the stream, so the
	// next frame is the real next frame and not a shifted one.
	if _, err := ring.Write([]byte{8}); err != nil {
		t.Fatalf("write: %v", err)
	}
	n, _ = ring.ReadAvailable(buf)
	if n != frame {
		t.Fatalf("expected one frame after the gap was completed, got %d", n)
	}
	if got := buf[:n]; string(got) != string([]byte{5, 6, 7, 8}) {
		t.Fatalf("stream is byte-shifted: got %v, want [5 6 7 8]", got)
	}
}

// The tail of an item must never be stranded by the alignment rule.
func TestTheLastPartialFrameStillPlays(t *testing.T) {
	ring := NewRing(4096, 0, 4)
	if _, err := ring.Write([]byte{1, 2, 3, 4, 5, 6}); err != nil {
		t.Fatalf("write: %v", err)
	}
	ring.CloseWrite()
	buf := make([]byte, 64)
	total := 0
	for i := 0; i < 4; i++ {
		n, eof := ring.ReadAvailable(buf)
		total += n
		if eof {
			break
		}
	}
	if total != 6 {
		t.Fatalf("the end of the item was truncated: read %d of 6 bytes", total)
	}
}
