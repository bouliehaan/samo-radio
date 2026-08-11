package player

import "time"

// Mode is what the device is doing with the sound card.
type Mode string

const (
	// ModeIdle is standby: the sink is open and silent, on purpose.
	ModeIdle Mode = "idle"
	// ModeChannel is tuned to a Samo channel — the device's resting state.
	ModeChannel Mode = "channel"
	// ModeQueue is an ad-hoc list somebody sent from a client.
	ModeQueue Mode = "queue"
)

// Status is the transport state clients render.
type Status string

const (
	StatusIdle      Status = "idle"
	StatusPlaying   Status = "playing"
	StatusPaused    Status = "paused"
	StatusBuffering Status = "buffering"
	StatusError     Status = "error"
)

// Item is one playable thing.
//
// Everything here is resolved by samo-server before it reaches the device,
// including the absolute stream URL. The daemon deliberately knows nothing
// about Samo's catalog: teaching it to turn a track id into a URL would mean
// two implementations of that mapping drifting apart, and the server already
// has the only one that can be right.
type Item struct {
	Ref             string  `json:"ref"`
	Title           string  `json:"title"`
	Subtitle        string  `json:"subtitle,omitempty"`
	ArtworkURL      string  `json:"artworkUrl,omitempty"`
	StreamURL       string  `json:"streamUrl"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	Kind            string  `json:"kind,omitempty"`
	Live            bool    `json:"live,omitempty"`

	// GainDB levels this item against everything else the device plays: a
	// constant offset in decibels, applied to every sample equally.
	//
	// Samo works it out, not the daemon, and for the same reason the daemon is
	// handed a resolved URL rather than a track id. The number comes from an
	// EBU R128 measurement of the file, which needs the library, somewhere to
	// cache the answer, and a machine willing to spend CPU on analysis — all
	// of which the server has and none of which belong in a process whose one
	// job is keeping the sound card fed. Zero means "play it as it is".
	GainDB float64 `json:"gainDb,omitempty"`

	// LimitPeaks puts a true-peak limiter after the gain. Set only for the
	// unusual item whose peaks would otherwise exceed the ceiling once lifted
	// — a quiet recording with tall transients. Samo bounds how much of the
	// correction the limiter is allowed to absorb, so this can never amount to
	// squashing the item.
	LimitPeaks bool `json:"limitPeaks,omitempty"`

	// CeilingDBTP is the limiter's threshold, meaningful only with LimitPeaks.
	CeilingDBTP float64 `json:"ceilingDbtp,omitempty"`
}

// ChannelState describes the tuned station and what it is airing.
//
// The JSON key stays `channel` and the type keeps its name because clients
// render it identically for both kinds; `Kind` is what tells them whether they
// are looking at a Samo channel or an internet station.
type ChannelState struct {
	ID            string `json:"id"`
	Kind          string `json:"kind,omitempty"`
	Name          string `json:"name,omitempty"`
	Title         string `json:"title,omitempty"`
	Artist        string `json:"artist,omitempty"`
	SourceLabel   string `json:"sourceLabel,omitempty"`
	ListenerCount int    `json:"listenerCount,omitempty"`
}

// StationRef names what the device falls back to.
type StationRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// OutputState is the sound-card side of the status report.
type OutputState struct {
	Backend    string `json:"backend"`
	Device     string `json:"device,omitempty"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
	Open       bool   `json:"open"`
	Restarts   int64  `json:"restarts,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

// ServerState is the Samo side.
type ServerState struct {
	BaseURL string `json:"baseUrl,omitempty"`
	Name    string `json:"name,omitempty"`
	Paired  bool   `json:"paired"`
}

// State is the whole device in one object.
//
// Every response and every SSE frame carries a complete snapshot rather than a
// delta. A client that reconnects after a dropped connection needs no replay,
// which is what makes the phone's panel correct after it has been in a pocket
// for an hour.
type State struct {
	DeviceName      string        `json:"deviceName"`
	Mode            Mode          `json:"mode"`
	Status          Status        `json:"status"`
	Volume          float64       `json:"volume"`
	PositionSeconds float64       `json:"positionSeconds"`
	DurationSeconds float64       `json:"durationSeconds,omitempty"`
	Item            *Item         `json:"item,omitempty"`
	Queue           []Item        `json:"queue,omitempty"`
	QueueIndex      int           `json:"queueIndex"`
	Channel         *ChannelState `json:"channel,omitempty"`

	DefaultStation *StationRef `json:"defaultStation,omitempty"`

	Output OutputState `json:"output"`
	Server ServerState `json:"server"`

	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Version increments on every structural change, so a poller can tell a
	// new state from a repeat of the last one without diffing it.
	Version uint64 `json:"version"`
}
