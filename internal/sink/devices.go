package sink

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"time"
)

// Device is one selectable audio output.
type Device struct {
	// ID is what goes in the config and on the sink command line.
	ID string `json:"id"`
	// Name is the human label ("HDA Intel PCH, ALC887-VD Analog").
	Name string `json:"name"`
	// Detail is the second descriptive line, when the tool provides one.
	Detail string `json:"detail,omitempty"`
	// Recommended flags the entries an operator almost always wants: the
	// analog line-out through ALSA's format-converting plug layer.
	Recommended bool `json:"recommended,omitempty"`
}

// ListDevices enumerates the outputs the resolved backend can play to.
//
// This is what makes "which socket is the aux port" answerable from the phone
// instead of over SSH: the daemon shells out to the same tools an operator
// would run by hand and hands the parsed list to the UI.
func ListDevices(ctx context.Context, backend Backend) ([]Device, Backend, error) {
	resolved, err := ResolveBackend(backend)
	if err != nil {
		return nil, backend, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	switch resolved {
	case BackendPulse:
		devices, err := listPulseDevices(ctx)
		return devices, resolved, err
	case BackendALSA:
		devices, err := listALSADevices(ctx)
		return devices, resolved, err
	default:
		// A custom command owns its own routing; there is nothing to enumerate.
		return []Device{}, resolved, nil
	}
}

// listALSADevices parses `aplay -L`, whose output is a flat list of PCM names
// each followed by indented description lines.
func listALSADevices(ctx context.Context) ([]Device, error) {
	output, err := exec.CommandContext(ctx, "aplay", "-L").Output()
	if err != nil {
		return nil, err
	}
	var devices []Device
	var current *Device
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if current == nil {
				continue
			}
			text := strings.TrimSpace(line)
			if current.Name == "" {
				current.Name = text
			} else if current.Detail == "" {
				current.Detail = text
			}
			continue
		}
		if current != nil {
			devices = append(devices, *current)
		}
		id := strings.TrimSpace(line)
		current = &Device{ID: id, Recommended: isRecommendedALSA(id)}
	}
	if current != nil {
		devices = append(devices, *current)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].Name == "" {
			devices[i].Name = devices[i].ID
		}
	}
	return devices, nil
}

// isRecommendedALSA marks the entries worth trying first.
//
// plughw: is the plug layer over a raw device — it converts rate and format in
// software, so a card that only does 44.1kHz still accepts our 48kHz stream
// instead of failing to open. hw: is the same card with no conversion, which is
// how you get "Sample format non available" on a perfectly good sound card.
func isRecommendedALSA(id string) bool {
	switch {
	case id == "default":
		return true
	case strings.HasPrefix(id, "plughw:"):
		return true
	case strings.HasPrefix(id, "sysdefault:"):
		return true
	default:
		return false
	}
}

// listPulseDevices parses `pactl list short sinks`: index, name, driver, spec,
// state, tab-separated.
func listPulseDevices(ctx context.Context) ([]Device, error) {
	output, err := exec.CommandContext(ctx, "pactl", "list", "short", "sinks").Output()
	if err != nil {
		return nil, err
	}
	var devices []Device
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSpace(fields[1])
		if name == "" {
			continue
		}
		device := Device{ID: name, Name: name, Recommended: strings.Contains(strings.ToLower(name), "analog")}
		if len(fields) >= 4 {
			device.Detail = strings.TrimSpace(fields[3])
		}
		devices = append(devices, device)
	}
	return devices, scanner.Err()
}
