// Package config holds samo-radio's on-disk state: where Samo is, how to
// authenticate to it, which socket the audio comes out of, and what to play
// when nobody has said otherwise.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultPath is where the systemd unit points. It lives under the daemon's
// own state directory rather than /etc because the daemon writes it: pairing
// and settings changes come in over the API, not from an operator with an
// editor.
const DefaultPath = "/var/lib/samo-radio/config.json"

// DefaultListenAddr binds every interface.
//
// The device is not necessarily on the same machine as Samo. A Pi with an amp
// on the other side of the house is the setup this daemon is for, and a
// loopback-only default would make that box unreachable out of the box — the
// first thing anyone did would be to SSH in and edit JSON, which is exactly
// the errand this project exists to remove.
//
// What keeps it safe on a LAN is the control token, not the bind address: it
// is required on every route, and the daemon mints one on first run rather
// than leaving the choice to whoever installs it. Pin this back to
// 127.0.0.1:7970 when Samo is on the same box and nothing else should reach it.
const DefaultListenAddr = "0.0.0.0:7970"

// Server is the Samo instance this device pulls audio from.
type Server struct {
	BaseURL string `json:"baseUrl"`
	// Token is a real Samo API token, minted by the server during pairing and
	// held here for the device's lifetime.
	//
	// A short-lived stream token would be wrong for this box: it is supposed to
	// come up after a power cut and start playing with nobody watching, and to
	// hold a channel open for weeks. Both need a credential that outlives the
	// request that created it.
	Token string `json:"token"`
	// ServerName is cosmetic — it makes the device's own status page readable.
	ServerName string `json:"serverName,omitempty"`
}

// Station kinds. A station is anything the device can sit on indefinitely.
const (
	// StationChannel is a Samo channel — programmed 24/7 radio.
	StationChannel = "channel"
	// StationInternet is an internet radio station from Samo's catalog. Same
	// idea from the device's point of view: an endless live stream it tunes.
	StationInternet = "station"
)

// Station is what the device falls back to: a channel or an internet station.
type Station struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Name is cached so the device can label what it is playing without a
	// lookup — it has to come back on air after a power cut with the server
	// possibly still starting up.
	Name string `json:"name,omitempty"`
}

// Set reports whether a station has been chosen.
func (s Station) Set() bool { return strings.TrimSpace(s.ID) != "" }

// Output describes the sound card side.
type Output struct {
	Backend      string   `json:"backend"`
	Device       string   `json:"device"`
	SampleRate   int      `json:"sampleRate"`
	Channels     int      `json:"channels"`
	BufferMillis int      `json:"bufferMillis"`
	Command      []string `json:"command,omitempty"`
}

// Config is the whole persisted state.
type Config struct {
	DeviceName   string `json:"deviceName"`
	ListenAddr   string `json:"listenAddr"`
	ControlToken string `json:"controlToken,omitempty"`

	Server Server `json:"server"`
	Output Output `json:"output"`

	Volume float64 `json:"volume"`

	// DefaultStation is what this device falls back to. It is what makes the
	// aux port a radio rather than a speaker: on boot, and whenever an ad-hoc
	// queue runs out, playback returns here.
	DefaultStation Station `json:"defaultStation"`
	AutoTuneOnBoot bool    `json:"autoTuneOnBoot"`

	// Superseded by DefaultStation, read once so a device configured before
	// internet stations were supported keeps its fallback across the upgrade
	// instead of coming back silent.
	LegacyDefaultChannelID   string `json:"defaultChannelId,omitempty"`
	LegacyDefaultChannelName string `json:"defaultChannelName,omitempty"`

	FFmpegPath     string  `json:"ffmpegPath,omitempty"`
	BufferSeconds  float64 `json:"bufferSeconds,omitempty"`
	PrefillSeconds float64 `json:"prefillSeconds,omitempty"`
}

// Defaults is a fresh, unpaired device.
func Defaults() Config {
	hostname, _ := os.Hostname()
	name := strings.TrimSpace(hostname)
	if name == "" {
		name = "samo-radio"
	}
	return Config{
		DeviceName: name,
		ListenAddr: DefaultListenAddr,
		Output: Output{
			Backend:      "auto",
			SampleRate:   48000,
			Channels:     2,
			BufferMillis: 300,
		},
		Volume:         1,
		AutoTuneOnBoot: true,
		BufferSeconds:  8,
		PrefillSeconds: 1,
	}
}

func (c *Config) normalize() {
	if strings.TrimSpace(c.DeviceName) == "" {
		c.DeviceName = Defaults().DeviceName
	}
	c.ListenAddr = normalizeListenAddr(c.ListenAddr)
	if strings.TrimSpace(c.Output.Backend) == "" {
		c.Output.Backend = "auto"
	}
	if c.Output.SampleRate <= 0 {
		c.Output.SampleRate = 48000
	}
	if c.Output.Channels <= 0 {
		c.Output.Channels = 2
	}
	if c.Output.BufferMillis <= 0 {
		c.Output.BufferMillis = 300
	}
	// Clamp, never re-default.
	//
	// normalize() runs on every write, so `<= 0 -> 1` here did not fill in a
	// missing level, it overruled a deliberate one: setting the volume to zero
	// persisted as 1.0, the sink really was silenced, and the daemon then
	// reported 100% to everything that asked. Every client's next render
	// snapped its slider to full, and the next openSink re-applied the stored
	// 1.0 — so a deliberately muted device came back at full blast.
	//
	// An ABSENT volume key never needed this: Load starts from Defaults()
	// (Volume: 1) and unmarshals over it, which is the one place that can tell
	// "not set" from "set to zero".
	if c.Volume < 0 {
		c.Volume = 0
	}
	if c.Volume > 1 {
		c.Volume = 1
	}
	if c.BufferSeconds <= 0 {
		c.BufferSeconds = 8
	}
	if c.PrefillSeconds <= 0 {
		c.PrefillSeconds = 1
	}
	c.Server.BaseURL = strings.TrimRight(strings.TrimSpace(c.Server.BaseURL), "/")

	// Carry a pre-station-kinds config forward, then stop reading the old keys.
	if !c.DefaultStation.Set() && strings.TrimSpace(c.LegacyDefaultChannelID) != "" {
		c.DefaultStation = Station{
			Kind: StationChannel,
			ID:   strings.TrimSpace(c.LegacyDefaultChannelID),
			Name: strings.TrimSpace(c.LegacyDefaultChannelName),
		}
	}
	c.LegacyDefaultChannelID = ""
	c.LegacyDefaultChannelName = ""

	c.DefaultStation.ID = strings.TrimSpace(c.DefaultStation.ID)
	c.DefaultStation.Name = strings.TrimSpace(c.DefaultStation.Name)
	// An id with no kind is a channel: that is all this field could mean
	// before internet stations were an option.
	if c.DefaultStation.Kind != StationInternet {
		c.DefaultStation.Kind = StationChannel
	}
}

// Paired reports whether the device knows how to reach Samo.
func (c Config) Paired() bool {
	return c.Server.BaseURL != "" && strings.TrimSpace(c.Server.Token) != ""
}

// normalizeListenAddr accepts what a person actually types — "7970", ":7970",
// "0.0.0.0:7970", "192.168.1.42:7970" — and settles on host:port.
//
// A bare port is the common case in a drop-in or an env var, and reading it as
// a hostname would bind nothing at all.
func normalizeListenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	switch {
	case addr == "":
		return DefaultListenAddr
	case !strings.Contains(addr, ":"):
		return "0.0.0.0:" + addr
	case strings.HasPrefix(addr, ":"):
		return "0.0.0.0" + addr
	default:
		return addr
	}
}

// LoopbackOnly reports whether the listen address is reachable from this
// machine alone, which decides whether the daemon has anything to advertise.
func (c Config) LoopbackOnly() bool {
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// A hostname. Unresolvable here without a lookup, and "localhost" is
		// the only one anybody means as loopback.
		return strings.EqualFold(host, "localhost")
	}
	return ip.IsLoopback()
}

// EnsureControlToken mints the shared secret if the device does not have one,
// and persists it.
//
// The daemon does this rather than the installer because the installer is not
// the only way this ends up on a box: cloning the repo onto a Pi and running
// the binary has to produce a device that is protected, not one that takes
// orders from anything that can reach port 7970. Minting once and keeping it
// means an upgrade never invalidates the token already pasted into Samo.
func (s *Store) EnsureControlToken() (string, bool, error) {
	if existing := strings.TrimSpace(s.Snapshot().ControlToken); existing != "" {
		return existing, false, nil
	}
	var (
		token   string
		created bool
	)
	if _, err := s.Update(func(c *Config) error {
		if existing := strings.TrimSpace(c.ControlToken); existing != "" {
			token = existing
			return nil
		}
		minted, err := newControlToken()
		if err != nil {
			return err
		}
		c.ControlToken = minted
		token, created = minted, true
		return nil
	}); err != nil {
		return "", false, err
	}
	return token, created, nil
}

func newControlToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Store is the concurrency-safe owner of the config file. The API mutates it
// while the player reads it, so nothing outside this package touches a Config
// it did not get from Snapshot.
type Store struct {
	mu    sync.RWMutex
	path  string
	value Config
}

// Load reads the config, creating it with defaults if absent, then layers
// environment overrides on top.
//
// Env wins over file so a systemd drop-in can pin something (the listen
// address, the control token) without fighting the daemon's own writes.
func Load(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	value := Defaults()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// First boot. The file gets written on the first save.
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	applyEnv(&value)
	value.normalize()
	store := &Store{path: path, value: value}
	return store, nil
}

func applyEnv(c *Config) {
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_NAME")); v != "" {
		c.DeviceName = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_ADDR")); v != "" {
		c.ListenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_CONTROL_TOKEN")); v != "" {
		c.ControlToken = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_SERVER_URL")); v != "" {
		c.Server.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_SERVER_TOKEN")); v != "" {
		c.Server.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_BACKEND")); v != "" {
		c.Output.Backend = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_DEVICE")); v != "" {
		c.Output.Device = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_FFMPEG")); v != "" {
		c.FFmpegPath = v
	}
	if v := strings.TrimSpace(os.Getenv("SAMO_RADIO_SAMPLE_RATE")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.Output.SampleRate = parsed
		}
	}
}

// Path is where this store persists.
func (s *Store) Path() string { return s.path }

// Snapshot returns a copy safe to read without holding the lock.
func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// Update mutates the config under lock and persists it. The mutation runs
// inside the lock so read-modify-write from two API calls cannot interleave.
func (s *Store) Update(mutate func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.value
	if err := mutate(&next); err != nil {
		return s.value, err
	}
	next.normalize()
	if err := write(s.path, next); err != nil {
		return s.value, err
	}
	s.value = next
	return next, nil
}

// write persists atomically: a torn config file on a power cut would leave the
// device unable to reach Samo, which is the one failure it cannot recover from
// on its own.
func write(path string, value Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temp := path + ".tmp"
	// 0600: this file holds a Samo API token and the control secret.
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
