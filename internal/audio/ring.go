package audio

import (
	"errors"
	"sync"
)

// ErrRingClosed is returned to a writer that keeps producing after the ring
// has been closed for reading (the track was skipped, the queue moved on).
var ErrRingClosed = errors.New("audio: ring closed")

// Ring is the jitter buffer between one decoder and the sink.
//
// It exists because the two ends have incompatible timing contracts. The sound
// card is a hard real-time consumer: it drains at exactly BytesPerSecond and
// anything that fails to feed it on time is an audible dropout. A decoder
// reading an HTTP stream is not real-time at all — it stalls whenever the
// network does. Reading the decoder directly from the sink pump would hand
// every network hiccup straight to the speaker, and worse, would block the pump
// mid-frame with the card's buffer running dry.
//
// So writes block (which backpressures ffmpeg — it fills the buffer as fast as
// the network allows, then waits) and reads never block: an empty ring returns
// zero bytes and the pump writes silence for that frame. A dropout becomes a
// recoverable gap instead of a stalled device.
type Ring struct {
	mu     sync.Mutex
	space  *sync.Cond
	data   []byte
	start  int
	length int

	// writerDone means "no more bytes are coming". The ring is only truly at
	// EOF once that is set AND everything buffered has been read out, so the
	// tail of a track is never truncated.
	writerDone bool
	closed     bool

	// prefill is how much has to accumulate before reads are allowed to start.
	// Without it, playback begins on the first few bytes and immediately
	// starves, which sounds far worse than waiting a moment to start.
	prefill   int
	prefilled bool

	// frame is the PCM frame size, and reads are kept on that boundary.
	//
	// The writer is a pipe, so it delivers whatever byte count the kernel felt
	// like — routinely not a whole number of frames. That is harmless while the
	// ring is full, because a read is then capped at the caller's frame-sized
	// buffer. The moment the ring runs dry the read returns everything it has,
	// which may be a partial frame, and the read cursor moves by that amount.
	// From then on every frame handed to the card starts mid-frame: on 16-bit
	// stereo an odd remainder byte-swaps every sample, which is full-scale
	// white noise, and it does not heal until the item ends and the ring is
	// rebuilt.
	frame int
}

// NewRing allocates a ring holding capacity bytes, gated by prefill bytes.
// frame is the PCM frame size in bytes; reads are kept on that boundary. Pass
// 0 or 1 for a ring carrying bytes that have no frame structure.
func NewRing(capacity, prefill, frame int) *Ring {
	if capacity <= 0 {
		capacity = 1 << 16
	}
	if prefill < 0 {
		prefill = 0
	}
	if prefill > capacity {
		prefill = capacity
	}
	if frame < 1 {
		frame = 1
	}
	r := &Ring{data: make([]byte, capacity), prefill: prefill, frame: frame}
	r.space = sync.NewCond(&r.mu)
	return r
}

// Write blocks until every byte of p is buffered, or the ring is closed.
// This is the backpressure that keeps a decoder from racing ahead of playback
// and buffering an entire album in memory.
func (r *Ring) Write(p []byte) (int, error) {
	written := 0
	r.mu.Lock()
	defer r.mu.Unlock()
	for written < len(p) {
		for r.length == len(r.data) && !r.closed {
			r.space.Wait()
		}
		if r.closed {
			return written, ErrRingClosed
		}
		n := r.writeLocked(p[written:])
		written += n
	}
	return written, nil
}

func (r *Ring) writeLocked(p []byte) int {
	free := len(r.data) - r.length
	if free > len(p) {
		free = len(p)
	}
	end := (r.start + r.length) % len(r.data)
	first := len(r.data) - end
	if first > free {
		first = free
	}
	copy(r.data[end:end+first], p[:first])
	if first < free {
		copy(r.data[:free-first], p[first:free])
	}
	r.length += free
	return free
}

// ReadAvailable fills as much of p as the ring currently holds and returns
// immediately. It never blocks, because its caller is the sink pump and a
// blocked pump is a starved sound card.
//
// eof reports that the writer finished and the buffer is drained — the signal
// the player uses to advance to the next item.
func (r *Ring) ReadAvailable(p []byte) (n int, eof bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, true
	}
	// Hold playback until the buffer has a cushion, unless the source already
	// ended (a clip shorter than the prefill target must still play).
	if !r.prefilled {
		if r.length < r.prefill && !r.writerDone {
			return 0, false
		}
		r.prefilled = true
	}
	n = r.length
	if n > len(p) {
		n = len(p)
	}
	// Stay on the frame boundary. Once the writer has finished there is no more
	// audio coming, so the last partial frame is handed over rather than
	// stranded — that tail is the end of the item, not a misalignment that
	// anything later has to live with.
	if r.frame > 1 && !r.writerDone {
		n -= n % r.frame
	}
	if n > 0 {
		first := len(r.data) - r.start
		if first > n {
			first = n
		}
		copy(p[:first], r.data[r.start:r.start+first])
		if first < n {
			copy(p[first:n], r.data[:n-first])
		}
		r.start = (r.start + n) % len(r.data)
		r.length -= n
		r.space.Broadcast()
	}
	return n, r.writerDone && r.length == 0
}

// Buffered reports how many bytes are ready to play. The player turns this into
// the "rebuffering" flag clients see.
func (r *Ring) Buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.length
}

// CloseWrite marks the end of the source. Buffered audio still plays out.
func (r *Ring) CloseWrite() {
	r.mu.Lock()
	r.writerDone = true
	r.mu.Unlock()
}

// Close abandons the ring in both directions, waking any blocked writer. Used
// when a track is skipped and its decoder needs to die.
func (r *Ring) Close() {
	r.mu.Lock()
	r.closed = true
	r.writerDone = true
	r.mu.Unlock()
	r.space.Broadcast()
}
