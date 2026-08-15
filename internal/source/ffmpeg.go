// Package source turns anything Samo can stream into raw PCM.
package source

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bouliehaan/samo-radio/internal/audio"
)

// Options describe one decode.
type Options struct {
	// URL is anything ffmpeg can open: an https stream from Samo, an icecast
	// URL, or a local file path.
	URL string

	// Headers ride on the request. In practice this is the Authorization
	// header carrying the device's Samo token.
	Headers map[string]string

	// StartSeconds seeks before decoding. Ignored for live sources, which have
	// no meaningful zero point.
	StartSeconds float64

	// Live marks an endless source (a channel, an internet station). Live
	// sources get ffmpeg's own reconnect logic and skip input seeking.
	Live bool

	// GainDB is a constant level offset for this item, in decibels, so items
	// mastered at wildly different levels come out of the aux port at the
	// same perceived loudness. Samo measures and decides; this end only
	// applies. Zero passes the audio through untouched.
	//
	// It is a single multiplication applied equally to every sample, which is
	// what makes it safe: the item's own dynamics — the gap between its quiet
	// and loud passages — are exactly as the engineer left them. Nothing here
	// compresses anything.
	GainDB float64

	// LimitPeaks inserts a true-peak limiter after the gain, for the rare
	// quiet-but-peaky item that would otherwise overshoot when lifted.
	LimitPeaks bool

	// CeilingDBTP is the limiter threshold in dBTP. Zero uses -1.
	CeilingDBTP float64

	Format         audio.Format
	FFmpegPath     string
	BufferSeconds  float64
	PrefillSeconds float64
	Logger         *log.Logger
}

// defaultCeilingDBTP leaves a decibel of headroom below full scale. The sink
// works in 16-bit integers and clips hard, so a boosted item that reaches
// exactly 0 dBFS has nowhere to go.
const defaultCeilingDBTP = -1.0

// Decoder is a running ffmpeg subprocess writing PCM into a ring.
type Decoder struct {
	ring    *audio.Ring
	cmd     *exec.Cmd
	stderr  *tailBuffer
	logger  *log.Logger
	failure atomic.Pointer[string]
	done    chan struct{}
	once    sync.Once
}

// Start launches ffmpeg and begins filling the ring. It returns as soon as the
// process is spawned — the caller hands Ring() to the sink and lets the ring's
// prefill gate decide when audio actually begins.
func Start(ctx context.Context, opts Options) (*Decoder, error) {
	format := opts.Format
	if format.SampleRate == 0 {
		format = audio.DefaultFormat
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	binary := strings.TrimSpace(opts.FFmpegPath)
	if binary == "" {
		binary = "ffmpeg"
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	bufferSeconds := opts.BufferSeconds
	if bufferSeconds <= 0 {
		bufferSeconds = 8
	}
	prefillSeconds := opts.PrefillSeconds
	if prefillSeconds <= 0 {
		prefillSeconds = 1
	}
	if prefillSeconds > bufferSeconds {
		prefillSeconds = bufferSeconds / 2
	}

	cmd := exec.CommandContext(ctx, binary, buildArgs(opts, format)...)
	// ffmpeg reads stdin for interactive commands and will consume the
	// daemon's if given the chance; -nostdin plus an explicit nil is belt and
	// braces.
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	ring := audio.NewRing(
		format.BytesForDuration(bufferSeconds),
		format.BytesForDuration(prefillSeconds),
		format.BytesPerFrame(),
	)
	if opts.Live {
		// A live stream that has fallen behind is late, not incomplete: the
		// audio it missed went out while it was stalled and replaying it now
		// would leave the station permanently that far behind the clock. Let it
		// sit at twice the prefill cushion — enough headroom that ordinary
		// jitter never trims, small enough that a scheduled cut-in is heard
		// when the schedule says it happens. Capacity still absorbs the stall.
		ring.CatchUpAt(format.BytesForDuration(prefillSeconds * 2))
	}

	decoder := &Decoder{
		ring:   ring,
		cmd:    cmd,
		stderr: stderr,
		logger: logger,
		done:   make(chan struct{}),
	}

	go decoder.pipe(stdout)
	return decoder, nil
}

// pipe copies ffmpeg's PCM into the ring and closes the write side when the
// source is exhausted. Ring writes block when full, which is what stops ffmpeg
// from decoding an entire album into memory ahead of playback.
func (d *Decoder) pipe(stdout io.ReadCloser) {
	defer close(d.done)
	buf := make([]byte, 64*1024)
	var copyErr error
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if _, writeErr := d.ring.Write(buf[:n]); writeErr != nil {
				// The ring was closed: this decode was abandoned (skip, stop,
				// new item). Not an error worth reporting.
				copyErr = nil
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				copyErr = readErr
			}
			break
		}
	}
	_ = stdout.Close()
	waitErr := d.cmd.Wait()
	d.ring.CloseWrite()

	if copyErr == nil && waitErr == nil {
		return
	}
	detail := strings.TrimSpace(d.stderr.String())
	message := "decode failed"
	switch {
	case copyErr != nil && detail != "":
		message = fmt.Sprintf("decode failed: %v: %s", copyErr, detail)
	case copyErr != nil:
		message = fmt.Sprintf("decode failed: %v", copyErr)
	case detail != "":
		message = fmt.Sprintf("ffmpeg exited: %v: %s", waitErr, detail)
	default:
		message = fmt.Sprintf("ffmpeg exited: %v", waitErr)
	}
	d.failure.Store(&message)
}

// Ring is the buffer the sink reads from.
func (d *Decoder) Ring() *audio.Ring { return d.ring }

// Done closes once ffmpeg has exited and the ring is sealed.
func (d *Decoder) Done() <-chan struct{} { return d.done }

// Err reports why the decode ended badly, or "" for a clean finish.
func (d *Decoder) Err() string {
	if p := d.failure.Load(); p != nil {
		return *p
	}
	return ""
}

// Close kills ffmpeg and abandons the ring.
func (d *Decoder) Close() {
	d.once.Do(func() {
		d.ring.Close()
		if d.cmd != nil && d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
	})
}

func buildArgs(opts Options, format audio.Format) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}

	if isHTTP(opts.URL) {
		if len(opts.Headers) > 0 {
			var builder strings.Builder
			for key, value := range opts.Headers {
				builder.WriteString(key)
				builder.WriteString(": ")
				builder.WriteString(value)
				builder.WriteString("\r\n")
			}
			args = append(args, "-headers", builder.String())
		}
		args = append(args, "-user_agent", "samo-radio/1")
		// Let ffmpeg ride out short network blips itself. The player only has
		// to get involved when the process actually gives up.
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "10",
		)
		if opts.Live {
			args = append(args, "-reconnect_at_eof", "1")
		}
	}

	// Input seeking (-ss before -i) is the fast one: ffmpeg jumps in the
	// container instead of decoding and discarding everything up to the mark.
	if !opts.Live && opts.StartSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(opts.StartSeconds, 'f', 3, 64))
	}

	args = append(args, "-i", opts.URL)

	// Cover art in an audio file is a video stream; without -vn ffmpeg tries to
	// map it and the raw muxer refuses the job.
	args = append(args, "-vn")

	// Levelling happens here rather than in the sink so the two gains stay
	// separate concerns: this one is programme level, decided per item and
	// fixed for its duration; the sink's is the listener's volume knob. Doing
	// it in ffmpeg also means the maths happens in float, before the s16
	// conversion, so a lift does not quantise.
	if filter := gainFilter(opts); filter != "" {
		args = append(args, "-af", filter)
	}

	args = append(args,
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"-ar", strconv.Itoa(format.SampleRate),
		"-ac", strconv.Itoa(format.Channels),
		"-",
	)
	return args
}

// gainFilter renders the item's level correction as an ffmpeg filtergraph, or
// "" when there is nothing to do.
func gainFilter(opts Options) string {
	var parts []string
	if opts.GainDB != 0 {
		parts = append(parts, fmt.Sprintf("volume=%.1fdB", opts.GainDB))
	}
	if opts.LimitPeaks {
		ceiling := opts.CeilingDBTP
		if ceiling == 0 {
			ceiling = defaultCeilingDBTP
		}
		// level=disabled is not optional: alimiter's default is to normalise
		// its output back up after limiting, which would throw away the level
		// decision the gain just made and hand back a uniformly loud stream.
		parts = append(parts, fmt.Sprintf(
			"alimiter=limit=%.4f:attack=5:release=60:level=disabled",
			math.Pow(10, ceiling/20)))
	}
	return strings.Join(parts, ",")
}

func isHTTP(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
