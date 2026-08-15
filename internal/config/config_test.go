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
