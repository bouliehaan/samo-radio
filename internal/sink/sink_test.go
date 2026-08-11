package sink

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bouliehaan/samo-radio/internal/audio"
)

func TestApplyGainSilencesAndScales(t *testing.T) {
	frame := int16Frame(1000, -1000, 32767, -32768)
	applyGain(frame, 0, 0)
	for i, value := range readInt16(frame) {
		if value != 0 {
			t.Fatalf("sample %d should be silent at zero gain, got %d", i, value)
		}
	}

	frame = int16Frame(1000, -1000)
	applyGain(frame, 0.5, 0.5)
	got := readInt16(frame)
	if got[0] != 500 || got[1] != -500 {
		t.Fatalf("expected half amplitude, got %v", got)
	}
}

// A full-scale sample scaled up must clamp rather than wrap: an int16 overflow
// is not a loud sample, it is a violent click at the opposite polarity.
func TestApplyGainClamps(t *testing.T) {
	frame := int16Frame(32767, -32768)
	applyGain(frame, 1, 1)
	got := readInt16(frame)
	if got[0] != 32767 || got[1] != -32768 {
		t.Fatalf("unity gain must be lossless, got %v", got)
	}
}

// The ramp is what keeps pause and volume changes from clicking; it has to
// actually move across the frame rather than jumping at the boundary.
func TestApplyGainRampsWithinFrame(t *testing.T) {
	samples := make([]int16, 8)
	for i := range samples {
		samples[i] = 1000
	}
	frame := int16Frame(samples...)
	applyGain(frame, 1, 0)
	got := readInt16(frame)
	if got[0] <= got[len(got)-1] {
		t.Fatalf("expected a descending ramp across the frame, got %v", got)
	}
	if got[0] != 1000 {
		t.Fatalf("ramp should start at the entry gain, got %d", got[0])
	}
}

func TestStepGainConverges(t *testing.T) {
	gain := 1.0
	for i := 0; i < 10; i++ {
		gain = stepGain(gain, 0, 0.34)
	}
	if gain != 0 {
		t.Fatalf("expected the ramp to reach zero, got %v", gain)
	}
	gain = stepGain(0, 1, 0.34)
	if gain != 0.34 {
		t.Fatalf("expected a single step of 0.34, got %v", gain)
	}
	if got := stepGain(0.9, 1, 0.34); got != 1 {
		t.Fatalf("expected the step to clamp at the target, got %v", got)
	}
}

func TestSinkCommandALSA(t *testing.T) {
	args, err := SinkCommand(BackendALSA, "plughw:CARD=PCH,DEV=0", audio.DefaultFormat, 300, nil)
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"aplay", "-t raw", "-f S16_LE", "-c 2", "-r 48000", "-D plughw:CARD=PCH,DEV=0", "--buffer-time=300000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in %q", want, joined)
		}
	}
	if args[len(args)-1] != "-" {
		t.Fatalf("aplay must read stdin, got %q", args[len(args)-1])
	}
}

// paplay is given no filename at all: some builds treat "-" as a literal path.
func TestSinkCommandPulseReadsStdinWithoutDash(t *testing.T) {
	args, err := SinkCommand(BackendPulse, "", audio.DefaultFormat, 250, nil)
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	for _, arg := range args {
		if arg == "-" {
			t.Fatalf("paplay must not be passed a dash: %v", args)
		}
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--raw") || !strings.Contains(joined, "--latency-msec=250") {
		t.Fatalf("unexpected paplay args: %q", joined)
	}
	if strings.Contains(joined, "--device=") {
		t.Fatalf("an empty device must not produce a --device flag: %q", joined)
	}
}

func TestSinkCommandCustomExpandsPlaceholders(t *testing.T) {
	args, err := SinkCommand(BackendCustom, "hw:1", audio.DefaultFormat, 300, []string{"mysink", "--rate={rate}", "--dev={device}", "--ch={channels}"})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	want := []string{"mysink", "--rate=48000", "--dev=hw:1", "--ch=2"}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, args[i], want[i])
		}
	}
	if _, err := SinkCommand(BackendCustom, "", audio.DefaultFormat, 300, nil); err == nil {
		t.Fatal("expected an error when a custom backend has no command")
	}
}

func TestParseBackend(t *testing.T) {
	for _, input := range []string{"", "auto", "ALSA", " pulse "} {
		if _, err := ParseBackend(input); err != nil {
			t.Fatalf("ParseBackend(%q): %v", input, err)
		}
	}
	if _, err := ParseBackend("jack"); err == nil {
		t.Fatal("expected an unknown backend to be rejected")
	}
}

// plughw converts format in software; hw does not, which is how a perfectly
// good card ends up refusing a 48kHz stream.
func TestALSARecommendationPrefersPlugDevices(t *testing.T) {
	if !isRecommendedALSA("plughw:CARD=PCH,DEV=0") || !isRecommendedALSA("default") {
		t.Fatal("plughw and default should be recommended")
	}
	if isRecommendedALSA("hw:CARD=PCH,DEV=0") {
		t.Fatal("raw hw devices should not be recommended")
	}
}

func int16Frame(values ...int16) []byte {
	out := make([]byte, len(values)*2)
	for i, value := range values {
		out[i*2] = byte(uint16(value))
		out[i*2+1] = byte(uint16(value) >> 8)
	}
	return out
}

func readInt16(frame []byte) []int16 {
	out := make([]int16, len(frame)/2)
	for i := range out {
		out[i] = int16(uint16(frame[i*2]) | uint16(frame[i*2+1])<<8)
	}
	return out
}

// The pace clock is the backstop for a sink that never blocks. It must not fire
// on a healthy device (which blocks first) but must throttle a free-running one.
func TestPaceClockThrottlesOnlyWhenAhead(t *testing.T) {
	clock := newPaceClock(audio.DefaultFormat)
	second := audio.DefaultFormat.BytesPerSecond()

	// One second of audio written instantly is still inside the allowed lead.
	if wait := clock.advance(second); wait != 0 {
		t.Fatalf("expected no throttle within the lead window, got %v", wait)
	}
	// Three seconds in, with no wall time elapsed, it has to wait.
	clock.advance(second)
	wait := clock.advance(second)
	if wait <= 0 {
		t.Fatal("expected a free-running sink to be throttled")
	}
	if wait > 2*time.Second {
		t.Fatalf("throttle should target the lead window, got %v", wait)
	}

	// A sink that blocked for real time is never throttled.
	clock.reset()
	clock.started = time.Now().Add(-10 * time.Second)
	if wait := clock.advance(second); wait != 0 {
		t.Fatalf("expected no throttle when behind wall time, got %v", wait)
	}
}

// Closing a sink must stop its pump, not just its subprocess.
//
// The pump used to run on the caller's context — the player's, which lives as
// long as the daemon. Closing the sink killed the process but left the loop
// running, and the loop reads a missing writer as "the card went away", so it
// respawned the output process on the OLD device every two seconds for ever.
// Changing output device therefore left a zombie holding the previous card
// open, which is exactly what makes the newly-chosen one fail to open.
func TestClosingASinkStopsItsPump(t *testing.T) {
	device, err := New(Options{
		Format:  audio.DefaultFormat,
		Backend: BackendCustom,
		Command: []string{"sh", "-c", "cat >/dev/null"},
	})
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	if err := device.Start(context.Background()); err != nil {
		t.Skipf("cannot spawn a test sink here: %v", err)
	}
	if err := device.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := device.restarts.Load()

	// The respawn path waits two seconds between attempts, so this is long
	// enough for a surviving pump to prove it is still alive.
	time.Sleep(2500 * time.Millisecond)

	if after := device.restarts.Load(); after != before {
		t.Fatalf("the pump kept respawning the device after Close (%d -> %d restarts)", before, after)
	}
	device.procMu.Lock()
	running := device.proc != nil
	device.procMu.Unlock()
	if running {
		t.Fatal("a closed sink still has an output process attached")
	}
}
