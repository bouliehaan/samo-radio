package sink

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The two groups differ in what a mute MEANS, which is what decides whether
// touching one is a fix or vandalism.

// gateControls sit in front of every output on the card. Muted or at zero
// here, nothing comes out of anything — there is no configuration in which
// that is what somebody wanted, so these are safe to fix.
var gateControls = []string{"Master", "PCM"}

// outputControls select which physical socket is live. A muted Headphone with
// nothing plugged into it is correct; a muted Speaker on a machine that uses
// its line-out is correct. Muting one of these is a routing decision, not a
// broken default, so they are only touched when EVERY one of them is silenced
// — which means no socket is live at all and there is no decision to respect.
var outputControls = []string{"Speaker", "Headphone", "Line Out", "Front", "Digital"}

// MixerControl is the state of one playback control.
type MixerControl struct {
	Name    string
	Muted   bool
	Percent int
	// HasVolume is false for switch-only controls (a plain mute toggle), which
	// can be unmuted but have no level to raise.
	HasVolume bool
}

// NeedsHelp reports a control that is silencing the output.
//
// Muted, or turned all the way down. Anything else is left alone: a level
// somebody chose is a decision, and an installer that "helpfully" resets it to
// full every upgrade is worse than one that does nothing.
func (c MixerControl) NeedsHelp() bool {
	return c.Muted || (c.HasVolume && c.Percent == 0)
}

// MixerChange records what was altered, so the installer can report it rather
// than silently reconfiguring somebody's audio.
type MixerChange struct {
	Control string
	Was     string
	Now     string
}

var (
	amixerPercent = regexp.MustCompile(`\[(\d+)%\]`)
	amixerSwitch  = regexp.MustCompile(`\[(on|off)\]`)
)

// parseMixerControl reads one `amixer sget` block.
func parseMixerControl(name, output string) MixerControl {
	control := MixerControl{Name: name, Percent: -1}

	if match := amixerPercent.FindStringSubmatch(output); match != nil {
		if value, err := strconv.Atoi(match[1]); err == nil {
			control.Percent = value
			control.HasVolume = true
		}
	}
	if !control.HasVolume {
		control.Percent = 0
	}

	// A control is muted if ANY channel is off: one silent side is still a
	// broken output, and unmuting an already-on channel costs nothing.
	for _, match := range amixerSwitch.FindAllStringSubmatch(output, -1) {
		if match[1] == "off" {
			control.Muted = true
			break
		}
	}
	return control
}

// mixerControlNames lists the simple controls a card actually has.
func mixerControlNames(ctx context.Context, card string) (map[string]bool, error) {
	out, err := exec.CommandContext(ctx, "amixer", "-c", card, "scontrols").Output()
	if err != nil {
		return nil, fmt.Errorf("list mixer controls on card %s: %w", card, err)
	}
	return parseControlNames(string(out)), nil
}

var scontrolName = regexp.MustCompile(`'([^']+)'`)

func parseControlNames(output string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		if match := scontrolName.FindStringSubmatch(line); match != nil {
			names[match[1]] = true
		}
	}
	return names
}

// MixerReport is what a mixer pass did, and what it deliberately did not do.
type MixerReport struct {
	Card    string
	Changed []MixerChange
	// LeftAlone are silenced outputs that were not touched because another
	// output is live, so the mute looks like a routing choice.
	LeftAlone []MixerControl
}

// EnsureAudible makes a card audible without overriding decisions.
//
// Two rules, because a mute means different things in different places:
//
//   - Gate controls (Master, PCM) sit in front of everything. Silenced there
//     is never intentional if you want sound, so they are fixed.
//   - Output controls pick which socket is live. They are fixed ONLY when all
//     of them are silenced, i.e. nothing is routed anywhere and there is no
//     choice to preserve. If even one is live, every mute among them is
//     treated as deliberate and reported instead.
//
// Levels are set in dB rather than percent: 0dB is unity on any codec that
// reports a dB scale, so it can never ask for gain above what the DAC is built
// to output. Percent is the fallback for controls with no dB scale.
func EnsureAudible(ctx context.Context, card string) (MixerReport, error) {
	card = strings.TrimSpace(card)
	if card == "" {
		card = "0"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	report := MixerReport{Card: card, Changed: []MixerChange{}, LeftAlone: []MixerControl{}}
	available, err := mixerControlNames(ctx, card)
	if err != nil {
		return report, err
	}

	read := func(name string) (MixerControl, bool) {
		out, err := exec.CommandContext(ctx, "amixer", "-c", card, "sget", name).Output()
		if err != nil {
			return MixerControl{}, false
		}
		return parseMixerControl(name, string(out)), true
	}
	apply := func(control MixerControl) {
		was := describeControl(control)
		if err := setControl(ctx, card, control.Name, control.HasVolume); err != nil {
			return
		}
		after := control
		if refreshed, ok := read(control.Name); ok {
			after = refreshed
		}
		report.Changed = append(report.Changed, MixerChange{
			Control: control.Name, Was: was, Now: describeControl(after),
		})
	}

	for _, name := range gateControls {
		if !available[name] {
			continue
		}
		if control, ok := read(name); ok && control.NeedsHelp() {
			apply(control)
		}
	}

	present := make([]MixerControl, 0, len(outputControls))
	for _, name := range outputControls {
		if !available[name] {
			continue
		}
		if control, ok := read(name); ok {
			present = append(present, control)
		}
	}
	fix, leave := planOutputFixes(present)
	for _, control := range fix {
		apply(control)
	}
	report.LeftAlone = leave
	return report, nil
}

// planOutputFixes decides which silenced outputs to touch.
//
// The whole judgement call, kept as a pure function so it can be tested
// without a sound card — it is the part that can be wrong in a way that
// silently breaks somebody's setup.
//
// If ANY output is live, every mute among the others is a routing decision
// (headphones unplugged, speakers deliberately dead) and nothing is touched.
// Only when every single output is silenced — a card in its shipped state,
// with no socket routed anywhere — is there no decision to preserve.
func planOutputFixes(present []MixerControl) (fix, leave []MixerControl) {
	fix = []MixerControl{}
	leave = []MixerControl{}

	silenced := make([]MixerControl, 0, len(present))
	for _, control := range present {
		if control.NeedsHelp() {
			silenced = append(silenced, control)
		}
	}
	if len(silenced) == 0 {
		return fix, leave
	}
	if len(silenced) == len(present) {
		return silenced, leave
	}
	return fix, silenced
}

func setControl(ctx context.Context, card, name string, hasVolume bool) error {
	if !hasVolume {
		return exec.CommandContext(ctx, "amixer", "-c", card, "sset", name, "unmute").Run()
	}
	// 0dB is unity. Codecs whose maximum is below unity clamp to their
	// maximum, which is what "as loud as this card goes" means there.
	if err := exec.CommandContext(ctx, "amixer", "-c", card, "sset", name, "0dB", "unmute").Run(); err == nil {
		return nil
	}
	return exec.CommandContext(ctx, "amixer", "-c", card, "sset", name, "100%", "unmute").Run()
}

func describeControl(control MixerControl) string {
	state := "on"
	if control.Muted {
		state = "muted"
	}
	if !control.HasVolume {
		return state
	}
	return fmt.Sprintf("%d%% %s", control.Percent, state)
}

// PersistMixer saves the card's mixer state so a reboot does not come back
// silent. Only worth calling when something actually changed.
func PersistMixer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "alsactl", "store").Run()
}

// CardFromDevice pulls the card id out of an ALSA PCM name, so the mixer work
// lands on the card actually being played to rather than on card 0.
//
// "plughw:CARD=PCH,DEV=0" -> "PCH". An empty or unparseable device means the
// default card.
func CardFromDevice(device string) string {
	device = strings.TrimSpace(device)
	if device == "" {
		return "0"
	}
	for _, part := range strings.Split(device, ",") {
		part = strings.TrimSpace(part)
		// CARD= is not at the start: the plug layer prefixes it, as in
		// "plughw:CARD=PCH". Look for it anywhere in the segment.
		if _, after, ok := strings.Cut(part, "CARD="); ok {
			if card := strings.TrimSpace(after); card != "" {
				return card
			}
		}
		// Bare "hw:1" / "plughw:2" forms.
		if _, rest, ok := strings.Cut(part, ":"); ok {
			if _, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				return strings.TrimSpace(rest)
			}
		}
	}
	return "0"
}
