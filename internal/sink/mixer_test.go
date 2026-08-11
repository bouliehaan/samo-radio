package sink

import "testing"

// Real `amixer sget` output. A card that plays nothing usually looks exactly
// like one of these, and telling them apart is the whole job.
const (
	masterUnmuted = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 87
  Mono:
  Front Left: Playback 87 [100%] [0.00dB] [on]
  Front Right: Playback 87 [100%] [0.00dB] [on]
`
	masterMuted = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 87
  Mono:
  Front Left: Playback 87 [100%] [0.00dB] [off]
  Front Right: Playback 87 [100%] [0.00dB] [off]
`
	masterZeroed = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 87
  Mono:
  Front Left: Playback 0 [0%] [-65.25dB] [on]
  Front Right: Playback 0 [0%] [-65.25dB] [on]
`
	masterHalf = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 87
  Mono:
  Front Left: Playback 44 [51%] [-32.25dB] [on]
  Front Right: Playback 44 [51%] [-32.25dB] [on]
`
	oneChannelMuted = `Simple mixer control 'Speaker',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 87
  Mono:
  Front Left: Playback 87 [100%] [0.00dB] [on]
  Front Right: Playback 87 [100%] [0.00dB] [off]
`
	switchOnly = `Simple mixer control 'Auto-Mute Mode',0
  Capabilities: pswitch pswitch-joined
  Playback channels: Mono
  Mono: Playback [off]
`
)

func TestParseMixerControlReadsStateAndLevel(t *testing.T) {
	on := parseMixerControl("Master", masterUnmuted)
	if on.Muted || on.Percent != 100 || !on.HasVolume {
		t.Fatalf("unmuted master misread: %+v", on)
	}
	muted := parseMixerControl("Master", masterMuted)
	if !muted.Muted || muted.Percent != 100 {
		t.Fatalf("muted master misread: %+v", muted)
	}
	zero := parseMixerControl("Master", masterZeroed)
	if zero.Muted || zero.Percent != 0 {
		t.Fatalf("zeroed master misread: %+v", zero)
	}
	// A switch-only control has no level to raise, only a mute to lift.
	only := parseMixerControl("Auto-Mute Mode", switchOnly)
	if only.HasVolume || !only.Muted {
		t.Fatalf("switch-only control misread: %+v", only)
	}
}

// The rule that decides whether a single control is silencing the output.
func TestNeedsHelpOnlyForSilence(t *testing.T) {
	cases := []struct {
		label  string
		output string
		want   bool
	}{
		{"muted at full", masterMuted, true},
		{"turned all the way down", masterZeroed, true},
		{"one channel muted is still broken output", oneChannelMuted, true},
		{"switch-only and off", switchOnly, true},
		{"already audible", masterUnmuted, false},
		// The important one: a deliberate half-volume must survive an
		// install, or every upgrade stamps on somebody's settings.
		{"deliberately set to half", masterHalf, false},
	}
	for _, tc := range cases {
		if got := parseMixerControl("x", tc.output).NeedsHelp(); got != tc.want {
			t.Fatalf("%s: NeedsHelp() = %v want %v", tc.label, got, tc.want)
		}
	}
}

func TestParseControlNames(t *testing.T) {
	names := parseControlNames(`Simple mixer control 'Master',0
Simple mixer control 'Headphone',0
Simple mixer control 'PCM',0
Simple mixer control 'Auto-Mute Mode',0
`)
	for _, want := range []string{"Master", "Headphone", "PCM", "Auto-Mute Mode"} {
		if !names[want] {
			t.Fatalf("missing control %q in %v", want, names)
		}
	}
	if names["Capture"] {
		t.Fatal("only listed controls should be present")
	}
}

// The mixer work has to land on the card actually being played to, not card 0,
// or a machine with HDMI on card 0 gets its analog out left muted.
func TestCardFromDevice(t *testing.T) {
	cases := map[string]string{
		"plughw:CARD=PCH,DEV=0": "PCH",
		"hw:CARD=NVidia,DEV=3":  "NVidia",
		"":                      "0",
		"default":               "0",
		"hw:1":                  "1",
		"plughw:2":              "2",
	}
	for device, want := range cases {
		if got := CardFromDevice(device); got != want {
			t.Fatalf("CardFromDevice(%q) = %q want %q", device, got, want)
		}
	}
}

func ctl(name string, muted bool, percent int) MixerControl {
	return MixerControl{HasVolume: true, Muted: muted, Name: name, Percent: percent}
}

func names(controls []MixerControl) []string {
	out := make([]string, len(controls))
	for i, c := range controls {
		out[i] = c.Name
	}
	return out
}

// The judgement call. Unmuting every silenced output would un-silence sockets
// people muted on purpose — headphones with nothing plugged in, speakers on a
// machine wired through its line-out.
func TestPlanOutputFixesRespectsRouting(t *testing.T) {
	t.Run("one live output means every other mute is deliberate", func(t *testing.T) {
		fix, leave := planOutputFixes([]MixerControl{
			ctl("Speaker", false, 100),
			ctl("Headphone", true, 100),
			ctl("Line Out", true, 100),
		})
		if len(fix) != 0 {
			t.Fatalf("nothing should be touched while a socket is live, got %v", names(fix))
		}
		if len(leave) != 2 {
			t.Fatalf("both mutes should be reported, got %v", names(leave))
		}
	})

	t.Run("every output dead is a shipped default, not a choice", func(t *testing.T) {
		fix, leave := planOutputFixes([]MixerControl{
			ctl("Speaker", true, 100),
			ctl("Headphone", true, 100),
		})
		if len(fix) != 2 {
			t.Fatalf("all silenced outputs should be fixed, got %v", names(fix))
		}
		if len(leave) != 0 {
			t.Fatalf("nothing to report when everything was fixed, got %v", names(leave))
		}
	})

	t.Run("a zeroed output counts as dead alongside a muted one", func(t *testing.T) {
		fix, _ := planOutputFixes([]MixerControl{
			ctl("Speaker", false, 0),
			ctl("Headphone", true, 100),
		})
		if len(fix) != 2 {
			t.Fatalf("expected both, got %v", names(fix))
		}
	})

	t.Run("everything already live is left completely alone", func(t *testing.T) {
		fix, leave := planOutputFixes([]MixerControl{
			ctl("Speaker", false, 100),
			ctl("Line Out", false, 51),
		})
		if len(fix) != 0 || len(leave) != 0 {
			t.Fatalf("expected no action, got fix=%v leave=%v", names(fix), names(leave))
		}
	})

	t.Run("a card with no output controls does nothing", func(t *testing.T) {
		fix, leave := planOutputFixes(nil)
		if len(fix) != 0 || len(leave) != 0 {
			t.Fatalf("expected no action, got fix=%v leave=%v", names(fix), names(leave))
		}
	})
}

// Master and PCM are a different question: they gate every output, so silence
// there is never a routing choice.
func TestGateAndOutputControlsAreDisjoint(t *testing.T) {
	gates := map[string]bool{}
	for _, name := range gateControls {
		gates[name] = true
	}
	for _, name := range outputControls {
		if gates[name] {
			t.Fatalf("%q is in both groups — it would be fixed by the gate rule and never reach the routing rule", name)
		}
	}
}
