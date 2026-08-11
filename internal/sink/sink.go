// Package sink owns the one thing samo-radio exists to hold open: a live
// handle on the server's sound card.
package sink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bouliehaan/samo-radio/internal/audio"
)

// Stream is one decoded item feeding the sink.
type Stream struct {
	// ID lets the player tell "the track I started" from "the track that just
	// ended", so a late EOF from a stream that was already replaced cannot
	// advance the queue a second time.
	ID string

	// Ring is the jitter buffer the decoder writes into.
	Ring *audio.Ring

	// StartSeconds is where in the item this decode began, so a seek reports
	// absolute position rather than time-since-ffmpeg-started.
	StartSeconds float64
}

// Options configure a Sink.
type Options struct {
	Format        audio.Format
	Backend       Backend
	Device        string
	Command       []string // only for BackendCustom
	BufferMillis  int
	Logger        *log.Logger
	OnStreamEnded func(streamID string)
}

// Sink writes PCM into the sound card, forever.
//
// The design decision that shapes everything else: the device is opened once at
// boot and never closed while the daemon runs. When there is nothing to play it
// is fed digital silence rather than being released. That costs a few hundred
// kB/s of zeroes and buys three things that matter on a box wired into an amp:
// no reopen race with whatever else might grab the card, no relay click or DC
// pop between tracks, and no "device or resource busy" failure mode that only
// shows up when you are not in the room.
//
// It also means the pump loop is the daemon's clock. Writing to the sound card
// blocks once its buffer is full, so a loop of "read a frame, write a frame"
// self-paces at exactly real time with no timers involved.
type Sink struct {
	format  audio.Format
	backend Backend
	device  string
	command []string
	bufMS   int
	logger  *log.Logger
	onEnded func(string)

	frameBytes int

	mu      sync.Mutex
	stream  *Stream
	paused  bool
	stopped bool

	// gain is the smoothed output level. Volume changes, pauses and resumes all
	// move the target and let the pump ramp toward it over a few frames, because
	// a hard cut to or from zero on a live PCM stream is an audible click.
	targetGain float64
	gain       float64

	consumed atomic.Int64 // real audio bytes played from the current stream
	starving atomic.Bool
	running  atomic.Bool
	lastErr  atomic.Pointer[string]
	restarts atomic.Int64

	writer io.WriteCloser
	proc   *exec.Cmd
	procMu sync.Mutex

	// stopPump ends this sink's own pump goroutine.
	//
	// The pump used to run on the CALLER's context, which is the player's and
	// lives as long as the daemon. Closing a sink therefore stopped the
	// subprocess but not the loop driving it — and the loop treats a missing
	// writer as "the card went away", so it respawned the sink process on the
	// OLD device every two seconds, for ever. Changing output device left a
	// zombie holding the previous card open, which is exactly what makes the
	// new one fail to open.
	//
	// A sink's pump belongs to the sink, so its lifetime does too.
	stopPump context.CancelFunc
}

// New builds a sink. Nothing is opened until Start.
func New(opts Options) (*Sink, error) {
	format := opts.Format
	if format.SampleRate == 0 {
		format = audio.DefaultFormat
	}
	if err := format.Validate(); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	backend := opts.Backend
	if backend == "" {
		backend = BackendAuto
	}
	bufMS := opts.BufferMillis
	if bufMS <= 0 {
		bufMS = 300
	}
	s := &Sink{
		format:  format,
		backend: backend,
		device:  opts.Device,
		command: opts.Command,
		bufMS:   bufMS,
		logger:  logger,
		onEnded: opts.OnStreamEnded,
		// 10ms frames: short enough that pause and volume feel instant, long
		// enough that the write syscall rate stays trivial.
		frameBytes: format.BytesForDuration(0.01),
		targetGain: 1,
		gain:       1,
	}
	if s.frameBytes <= 0 {
		s.frameBytes = format.BytesPerFrame()
	}
	return s, nil
}

// Start opens the device and begins the pump. It returns once the first sink
// process is running; the pump then keeps it alive for the process lifetime,
// respawning it if it dies.
func (s *Sink) Start(ctx context.Context) error {
	resolved, err := ResolveBackend(s.backend)
	if err != nil {
		return err
	}
	s.backend = resolved
	pumpCtx, cancel := context.WithCancel(ctx)
	if err := s.spawn(pumpCtx); err != nil {
		cancel()
		return err
	}
	s.procMu.Lock()
	if s.stopPump != nil {
		// Starting an already-started sink would otherwise strand the first
		// pump exactly the way Close used to.
		s.stopPump()
	}
	s.stopPump = cancel
	s.procMu.Unlock()
	go s.pump(pumpCtx)
	return nil
}

func (s *Sink) spawn(ctx context.Context) error {
	args, err := SinkCommand(s.backend, s.device, s.format, s.bufMS, s.command)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// The sink tools are chatty on stderr about buffer sizes; only surface it
	// when the process actually fails, which the pump logs.
	stderr := &tailBuffer{limit: 2048}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start audio sink (%s): %w", args[0], err)
	}
	s.procMu.Lock()
	s.proc = cmd
	s.writer = stdin
	s.procMu.Unlock()
	s.running.Store(true)
	s.setErr("")
	s.logger.Printf("audio sink open: %s device=%q %dHz %dch", args[0], s.deviceLabel(), s.format.SampleRate, s.format.Channels)
	go func() {
		err := cmd.Wait()
		s.running.Store(false)
		if ctx.Err() != nil {
			return
		}
		msg := "audio sink exited"
		if err != nil {
			msg = fmt.Sprintf("audio sink exited: %v: %s", err, stderr.String())
		}
		s.setErr(msg)
		s.logger.Printf("%s", msg)
	}()
	return nil
}

func (s *Sink) deviceLabel() string {
	if s.device == "" {
		return "default"
	}
	return s.device
}

// pump is the daemon's heartbeat: one frame in, one frame out, forever.
func (s *Sink) pump(ctx context.Context) {
	frame := make([]byte, s.frameBytes)
	// A frame's worth of ramp per 10ms means volume and pause changes settle in
	// ~30ms — inaudible as a transition, but enough to kill the click.
	const rampPerFrame = 0.34

	clock := newPaceClock(s.format)
	for ctx.Err() == nil {
		ended, endedID := s.fill(frame, rampPerFrame)
		if ended && s.onEnded != nil {
			s.onEnded(endedID)
		}
		if err := s.write(frame); err != nil {
			if ctx.Err() != nil {
				return
			}
			// The card went away (USB DAC unplugged, aplay killed, host audio
			// stack restarted). Keep the daemon alive and keep trying: this box
			// is meant to be a radio you never have to log into.
			s.setErr(fmt.Sprintf("audio sink write failed: %v", err))
			s.restarts.Add(1)
			s.closeProc()
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			if err := s.spawn(ctx); err != nil {
				s.setErr(fmt.Sprintf("audio sink reopen failed: %v", err))
			}
			clock.reset()
			continue
		}
		if ahead := clock.advance(len(frame)); ahead > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(ahead):
			}
		}
	}
}

// paceClock is a backstop for a sink that accepts writes without blocking.
//
// A real sound card is the pump's clock: its buffer fills and the write waits.
// Some outputs do not behave that way — ALSA's `null` device, a custom command
// piping to a file — and against one of those the loop free-runs, pegging a
// core and eating a whole track in milliseconds (measured: 10GB of PCM in five
// seconds). This keeps the loop honest by never letting it get more than a
// fraction of a second ahead of wall time. Against a device that does block it
// never fires, because the write already waited.
type paceClock struct {
	format  audio.Format
	started time.Time
	written int64
}

// leadSeconds is how far ahead of real time the pump may run before it waits.
// It has to exceed the sink's own buffering (a card legitimately takes a few
// hundred ms of audio up front) or this would throttle a healthy device.
const leadSeconds = 1.0

func newPaceClock(format audio.Format) *paceClock {
	return &paceClock{format: format, started: time.Now()}
}

func (p *paceClock) reset() {
	p.started = time.Now()
	p.written = 0
}

func (p *paceClock) advance(n int) time.Duration {
	p.written += int64(n)
	ahead := p.format.SecondsForBytes(p.written) - time.Since(p.started).Seconds()
	if ahead <= leadSeconds {
		return 0
	}
	return time.Duration((ahead - leadSeconds) * float64(time.Second))
}

// fill produces exactly one frame of output and reports whether the current
// stream just finished.
func (s *Sink) fill(frame []byte, rampPerFrame float64) (bool, string) {
	s.mu.Lock()
	stream := s.stream
	target := s.targetGain
	if s.paused || s.stopped || stream == nil {
		target = 0
	}
	gain := s.gain

	n := 0
	eof := false
	// Keep pulling audio while the ramp is still audible, so a pause fades the
	// real signal out instead of cutting to silence mid-waveform.
	if stream != nil && (target > 0 || gain > 0.0001) {
		n, eof = stream.Ring.ReadAvailable(frame)
	}
	// Account the bytes before releasing the stream, so the very last frame of
	// a track is counted against the track that produced it rather than
	// against whatever gets attached next.
	if n > 0 {
		s.consumed.Add(int64(n))
	}
	var endedID string
	if eof && stream != nil {
		endedID = stream.ID
		s.stream = nil
		s.consumed.Store(0)
	}
	nextGain := stepGain(gain, target, rampPerFrame)
	s.gain = nextGain
	s.mu.Unlock()

	if n > 0 {
		applyGain(frame[:n], gain, nextGain)
	}
	for i := n; i < len(frame); i++ {
		frame[i] = 0
	}
	// Starving means a stream is attached, wants to play, and had nothing
	// ready — the rebuffering signal clients render.
	s.starving.Store(stream != nil && !eof && n == 0 && target > 0)
	return endedID != "", endedID
}

func (s *Sink) write(frame []byte) error {
	s.procMu.Lock()
	w := s.writer
	s.procMu.Unlock()
	if w == nil {
		return errors.New("audio sink not open")
	}
	_, err := w.Write(frame)
	return err
}

func (s *Sink) closeProc() {
	s.procMu.Lock()
	if s.writer != nil {
		_ = s.writer.Close()
		s.writer = nil
	}
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Kill()
		s.proc = nil
	}
	s.procMu.Unlock()
	s.running.Store(false)
}

// SetStream swaps in a new decoded stream, discarding whatever was playing.
func (s *Sink) SetStream(stream *Stream) {
	s.mu.Lock()
	previous := s.stream
	s.stream = stream
	s.stopped = false
	s.paused = false
	s.mu.Unlock()
	s.consumed.Store(0)
	if previous != nil && previous.Ring != nil && (stream == nil || previous.Ring != stream.Ring) {
		previous.Ring.Close()
	}
}

// ClearStream stops playback and drops the current stream.
func (s *Sink) ClearStream() {
	s.SetStream(nil)
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

// SetPaused fades out and holds, or fades back in.
func (s *Sink) SetPaused(paused bool) {
	s.mu.Lock()
	s.paused = paused
	s.mu.Unlock()
}

// Paused reports the pause flag.
func (s *Sink) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// SetVolume sets the target output level, 0..1.
func (s *Sink) SetVolume(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	s.mu.Lock()
	s.targetGain = v
	s.mu.Unlock()
}

// Volume reports the target output level.
func (s *Sink) Volume() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targetGain
}

// PositionSeconds is where the current stream is, counted from bytes actually
// handed to the card rather than from a wall clock, so it stays honest through
// rebuffering and pauses.
func (s *Sink) PositionSeconds() float64 {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return 0
	}
	return stream.StartSeconds + s.format.SecondsForBytes(s.consumed.Load())
}

// Rebuffering reports that the current stream ran dry.
func (s *Sink) Rebuffering() bool { return s.starving.Load() }

// Running reports whether the sink process is alive.
func (s *Sink) Running() bool { return s.running.Load() }

// Restarts counts how many times the device had to be reopened.
func (s *Sink) Restarts() int64 { return s.restarts.Load() }

// LastError is the most recent device-level failure, or "".
func (s *Sink) LastError() string {
	if p := s.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// Format is the PCM shape the device was opened with.
func (s *Sink) Format() audio.Format { return s.format }

// Backend is the resolved output backend.
func (s *Sink) Backend() Backend { return s.backend }

// Device is the configured output device, "" for the backend default.
func (s *Sink) Device() string { return s.device }

// Close tears down the device, pump and all.
//
// The pump goes first. Killing the subprocess while its loop is still running
// just looks like a card that vanished, and the loop's whole job is to bring
// one of those back.
func (s *Sink) Close() error {
	s.procMu.Lock()
	stop := s.stopPump
	s.stopPump = nil
	s.procMu.Unlock()
	if stop != nil {
		stop()
	}
	s.ClearStream()
	s.closeProc()
	return nil
}

func (s *Sink) setErr(msg string) { s.lastErr.Store(&msg) }

func stepGain(current, target, step float64) float64 {
	if current < target {
		if current+step > target {
			return target
		}
		return current + step
	}
	if current-step < target {
		return target
	}
	return current - step
}

// applyGain scales a frame of S16_LE samples, interpolating from the frame's
// entry gain to its exit gain so a ramp is smooth within the frame too.
func applyGain(frame []byte, from, to float64) {
	if from == 1 && to == 1 {
		return
	}
	samples := len(frame) / audio.BytesPerSample
	if samples == 0 {
		return
	}
	stepPerSample := (to - from) / float64(samples)
	gain := from
	for i := 0; i < samples; i++ {
		offset := i * audio.BytesPerSample
		value := float64(int16(uint16(frame[offset]) | uint16(frame[offset+1])<<8))
		scaled := value * gain
		if scaled > 32767 {
			scaled = 32767
		} else if scaled < -32768 {
			scaled = -32768
		}
		out := uint16(int16(scaled))
		frame[offset] = byte(out)
		frame[offset+1] = byte(out >> 8)
		gain += stepPerSample
	}
}

// tailBuffer keeps the last N bytes of a subprocess's stderr so a failure can
// be reported with context without unbounded memory for a chatty tool.
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
