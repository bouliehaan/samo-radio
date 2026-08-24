package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A bare port is what people put in a drop-in or an env var, and reading it as
// a hostname would bind nothing at all.
func TestListenAddrAcceptsWhatPeopleType(t *testing.T) {
	for input, want := range map[string]string{
		"":                   DefaultListenAddr,
		"  ":                 DefaultListenAddr,
		"7970":               "0.0.0.0:7970",
		":7970":              "0.0.0.0:7970",
		"0.0.0.0:7970":       "0.0.0.0:7970",
		"127.0.0.1:7970":     "127.0.0.1:7970",
		"192.168.1.42:7970":  "192.168.1.42:7970",
		"[::]:7970":          "[::]:7970",
		"samo-pi.local:7970": "samo-pi.local:7970",
	} {
		if got := normalizeListenAddr(input); got != want {
			t.Fatalf("normalizeListenAddr(%q) = %q, want %q", input, got, want)
		}
	}
}

// The default has to be reachable from another machine: a Pi that only answers
// on its own loopback is a device nobody can add to Samo.
func TestDefaultsAreReachableFromTheNetwork(t *testing.T) {
	if Defaults().LoopbackOnly() {
		t.Fatalf("the default listen address %q cannot be reached off-box", Defaults().ListenAddr)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:7970":    true,
		"localhost:7970":    true,
		"[::1]:7970":        true,
		"0.0.0.0:7970":      false,
		"[::]:7970":         false,
		"192.168.1.42:7970": false,
	} {
		config := Config{ListenAddr: addr}
		if got := config.LoopbackOnly(); got != want {
			t.Fatalf("LoopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}

// A device that comes up without a control token would be taking orders from
// anything on the network, so it mints one — and keeps it, because the token
// already pasted into Samo has to survive a restart.
func TestControlTokenIsMintedOnceAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	token, created, err := store.EnsureControlToken()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created || token == "" {
		t.Fatalf("expected a freshly minted token, got %q (created=%v)", token, created)
	}

	again, created, err := store.EnsureControlToken()
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if created || again != token {
		t.Fatalf("token changed on a second call: %q -> %q (created=%v)", token, again, created)
	}

	// And across a restart, which is what an upgrade looks like.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.Snapshot().ControlToken; got != token {
		t.Fatalf("token did not survive a restart: %q -> %q", token, got)
	}
}

// The file holds a Samo API token and the control secret.
func TestConfigIsWrittenPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, _, err := store.EnsureControlToken(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config mode is %04o, want 0600", mode)
	}
}

// Mute has to survive being saved.
//
// normalize() runs on every write, and it used to turn a deliberate zero back
// into 1. The sink was silenced but the config said 100%, so every client's
// slider snapped to full on its next render — and the next openSink re-applied
// the stored 1.0 and played a muted device at full volume.
func TestMuteSurvivesAWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	saved, err := store.Update(func(c *Config) error {
		c.Volume = 0
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if saved.Volume != 0 {
		t.Fatalf("a deliberate mute was rewritten to %v", saved.Volume)
	}

	// And it is still zero after a restart, rather than being re-defaulted on
	// the way back in.
	reloaded, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.Snapshot().Volume; got != 0 {
		t.Fatalf("mute did not survive a reload, got %v", got)
	}
}

// A level nobody has ever set is still full volume — Load fills it from
// Defaults(), which is the only place that can tell "absent" from "zero".
func TestAnUnsetVolumeIsStillFull(t *testing.T) {
	store, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := store.Snapshot().Volume; got != 1 {
		t.Fatalf("a fresh config came up at %v, want 1", got)
	}
}

// Out-of-range levels are still clamped; only the re-defaulting went away.
func TestVolumeIsClamped(t *testing.T) {
	for input, want := range map[float64]float64{-0.5: 0, 0: 0, 0.25: 0.25, 1: 1, 4: 1} {
		c := Defaults()
		c.Volume = input
		c.normalize()
		if c.Volume != want {
			t.Fatalf("normalize volume %v = %v, want %v", input, c.Volume, want)
		}
	}
}
