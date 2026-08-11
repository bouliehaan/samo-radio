package sink

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bouliehaan/samo-radio/internal/audio"
)

// Backend selects how PCM reaches the sound card.
type Backend string

const (
	// BackendAuto picks Pulse when a sound server is actually running for this
	// process, and raw ALSA otherwise. On a headless box running as a systemd
	// *system* service there is no user session and therefore no sound server,
	// which is exactly when ALSA is both available and the right answer.
	BackendAuto Backend = "auto"

	// BackendALSA writes through aplay (alsa-utils). Direct, no daemon.
	BackendALSA Backend = "alsa"

	// BackendPulse writes through paplay (pulseaudio-utils), which also works
	// against PipeWire's Pulse server. Needed when something else already owns
	// the card and raw ALSA would get "device or resource busy".
	BackendPulse Backend = "pulse"

	// BackendCustom runs an operator-supplied command. The escape hatch for an
	// audio stack nobody anticipated; the command reads S16_LE on stdin.
	BackendCustom Backend = "custom"
)

// ErrNoBackend means neither aplay nor paplay is installed.
var ErrNoBackend = errors.New("no audio sink available: install alsa-utils (aplay) or pulseaudio-utils (paplay)")

// KnownBackends lists what the API will accept, for the settings UI.
func KnownBackends() []Backend {
	return []Backend{BackendAuto, BackendALSA, BackendPulse, BackendCustom}
}

// ParseBackend validates a backend name from config or the API.
func ParseBackend(raw string) (Backend, error) {
	value := Backend(strings.ToLower(strings.TrimSpace(raw)))
	if value == "" {
		return BackendAuto, nil
	}
	for _, known := range KnownBackends() {
		if value == known {
			return value, nil
		}
	}
	return "", fmt.Errorf("unknown audio backend %q", raw)
}

// ResolveBackend turns "auto" into a concrete backend and verifies the tool for
// an explicit one is actually installed, so a missing package fails at startup
// with a clear message instead of at the first play command.
func ResolveBackend(backend Backend) (Backend, error) {
	switch backend {
	case BackendCustom:
		return BackendCustom, nil
	case BackendALSA:
		if !hasTool("aplay") {
			return "", errors.New("audio backend alsa requires aplay (apt install alsa-utils)")
		}
		return BackendALSA, nil
	case BackendPulse:
		if !hasTool("paplay") {
			return "", errors.New("audio backend pulse requires paplay (apt install pulseaudio-utils)")
		}
		return BackendPulse, nil
	case BackendAuto, "":
		if soundServerRunning() && hasTool("paplay") {
			return BackendPulse, nil
		}
		if hasTool("aplay") {
			return BackendALSA, nil
		}
		if hasTool("paplay") {
			return BackendPulse, nil
		}
		return "", ErrNoBackend
	default:
		return "", fmt.Errorf("unknown audio backend %q", backend)
	}
}

// SinkCommand builds the argv for the long-lived sink process.
func SinkCommand(backend Backend, device string, format audio.Format, bufferMillis int, custom []string) ([]string, error) {
	switch backend {
	case BackendALSA:
		args := []string{
			"aplay",
			"-q",
			"-t", "raw",
			"-f", "S16_LE",
			"-c", strconv.Itoa(format.Channels),
			"-r", strconv.Itoa(format.SampleRate),
			// Buffer time is a latency/robustness trade: too small and a busy
			// server underruns audibly, too large and pause takes a beat to
			// take effect because that much audio is already in the card.
			"--buffer-time=" + strconv.Itoa(bufferMillis*1000),
		}
		if device != "" {
			args = append(args, "-D", device)
		}
		return append(args, "-"), nil
	case BackendPulse:
		args := []string{
			"paplay",
			"--raw",
			"--format=s16le",
			"--rate=" + strconv.Itoa(format.SampleRate),
			"--channels=" + strconv.Itoa(format.Channels),
			"--latency-msec=" + strconv.Itoa(bufferMillis),
			"--client-name=samo-radio",
			"--stream-name=samo-radio",
		}
		if device != "" {
			args = append(args, "--device="+device)
		}
		// paplay reads stdin when no file argument is given. It is deliberately
		// not passed "-": older builds treat that as a literal filename.
		return args, nil
	case BackendCustom:
		if len(custom) == 0 {
			return nil, errors.New("audio backend custom requires a command")
		}
		expanded := make([]string, len(custom))
		for i, part := range custom {
			replacer := strings.NewReplacer(
				"{rate}", strconv.Itoa(format.SampleRate),
				"{channels}", strconv.Itoa(format.Channels),
				"{device}", device,
				"{buffer_ms}", strconv.Itoa(bufferMillis),
			)
			expanded[i] = replacer.Replace(part)
		}
		return expanded, nil
	default:
		return nil, fmt.Errorf("unresolved audio backend %q", backend)
	}
}

func hasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// soundServerRunning reports whether a PulseAudio/PipeWire server is reachable
// from this process. Checking the socket rather than "is pipewire installed"
// matters: a system service has no user session, so the daemon may well be
// running for the desktop user and be completely unreachable from here.
func soundServerRunning() bool {
	if strings.TrimSpace(os.Getenv("PULSE_SERVER")) != "" {
		return true
	}
	runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if runtimeDir == "" {
		return false
	}
	for _, name := range []string{"pulse/native", "pipewire-0"} {
		if _, err := os.Stat(filepath.Join(runtimeDir, name)); err == nil {
			return true
		}
	}
	return false
}
