// Package samo is the daemon's client for the Samo server it belongs to.
//
// Deliberately small: samo-server hands this device fully-resolved stream URLs
// for anything ad hoc, so the only thing the daemon has to look up for itself
// is the channel it falls back to. That is the one case where it cannot ask —
// it has to be able to come up after a power cut, with nothing watching, and
// tune its default station on its own.
package samo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Channel mirrors the fields of samo-server's channel model the device needs.
type Channel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// PlaybackItem is what a channel is currently emitting.
type PlaybackItem struct {
	Title  string `json:"title"`
	Artist string `json:"artist,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// ArtworkURL pictures what is airing. Absolute for anything hosted
	// elsewhere, otherwise a samo-relative path for clients to join to the
	// server's base URL — the device only carries it, it never fetches it.
	ArtworkURL      string `json:"artworkUrl,omitempty"`
	SourceLabel     string `json:"sourceLabel,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	Live            bool   `json:"live,omitempty"`
}

// NowPlaying is the channel's now-playing summary.
type NowPlaying struct {
	ChannelID     string        `json:"channelId"`
	Current       *PlaybackItem `json:"current,omitempty"`
	StartedAt     *time.Time    `json:"startedAt,omitempty"`
	ListenerCount int           `json:"listenerCount"`
}

// Client talks to one Samo server with one token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client. An empty base URL or token yields a client whose calls
// all fail with ErrUnpaired, which is the honest state before pairing.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		// No timeout on the client itself: the same client is not used for
		// audio (ffmpeg does that), so every request here is a small JSON call
		// with its own context deadline.
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// ErrUnpaired means the device has no server to talk to yet.
var ErrUnpaired = fmt.Errorf("device is not paired with a Samo server")

// BaseURL is the configured server root.
func (c *Client) BaseURL() string { return c.baseURL }

// Paired reports whether this client has enough to make a request.
func (c *Client) Paired() bool { return c.baseURL != "" && c.token != "" }

// AuthHeaders are what ffmpeg must send to pull audio from this server.
func (c *Client) AuthHeaders() map[string]string {
	if c.token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + c.token}
}

// ChannelStreamURL is the long-lived audio pipe for a channel.
func (c *Client) ChannelStreamURL(channelID string) string {
	if c.baseURL == "" || channelID == "" {
		return ""
	}
	return c.baseURL + "/channels/" + url.PathEscape(channelID) + "/stream"
}

// InternetStationStreamURL is the audio pipe for a catalog internet station.
//
// Samo resolves the upstream URL behind this and 307s to it, so editing the
// station in Samo changes what the device plays with no reconfiguration here.
func (c *Client) InternetStationStreamURL(stationID string) string {
	if c.baseURL == "" || stationID == "" {
		return ""
	}
	return c.baseURL + "/internet-radio/" + url.PathEscape(stationID) + "/stream"
}

// InternetStation is a catalog internet radio station.
type InternetStation struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	// CoverURL is a picture uploaded into samo; ImageURL is the logo the
	// directory supplied. Preferring the first is what stationArtwork does.
	CoverURL   string `json:"coverUrl,omitempty"`
	ImageURL   string `json:"imageUrl,omitempty"`
	NowPlaying *struct {
		Title  string `json:"title,omitempty"`
		Artist string `json:"artist,omitempty"`
		Raw    string `json:"raw,omitempty"`
	} `json:"nowPlaying,omitempty"`
}

// InternetStationByID fetches one station, for its name and what it is airing.
//
// Samo probes stations for ICY metadata on its own schedule, so this is how a
// tuned station can say what is playing without the device parsing the stream.
func (c *Client) InternetStationByID(ctx context.Context, stationID string) (InternetStation, error) {
	var station InternetStation
	if stationID == "" {
		return station, fmt.Errorf("station id required")
	}
	err := c.get(ctx, "/api/v1/internet-radio/stations/"+url.PathEscape(stationID), &station)
	return station, err
}

// Channels lists the server's channels.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var payload struct {
		Items []Channel `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/channels", &payload); err != nil {
		return nil, err
	}
	return payload.Items, nil
}

// Channel fetches one channel, used to resolve a name for the fallback station.
func (c *Client) Channel(ctx context.Context, channelID string) (Channel, error) {
	var channel Channel
	if channelID == "" {
		return channel, fmt.Errorf("channel id required")
	}
	err := c.get(ctx, "/api/v1/channels/"+url.PathEscape(channelID), &channel)
	return channel, err
}

// NowPlaying reports what a channel is emitting right now.
//
// The device pulls one continuous MP3 pipe with no metadata in it, so this is
// the only way the aux port can say what is actually on.
func (c *Client) NowPlaying(ctx context.Context, channelID string) (NowPlaying, error) {
	var now NowPlaying
	if channelID == "" {
		return now, fmt.Errorf("channel id required")
	}
	err := c.get(ctx, "/api/v1/channels/"+url.PathEscape(channelID)+"/now", &now)
	return now, err
}

// SkipScope says how much of the programming to move past.
type SkipScope string

const (
	// SkipItem is "not this" — the station re-decides what belongs next.
	SkipItem SkipScope = "item"
	// SkipKind is "not this KIND of thing right now" — off the whole medium.
	SkipKind SkipScope = "kind"
)

// SkipChannel asks the server to move the channel's programming on.
//
// The device is only a listener: it holds one continuous pipe with no item
// boundaries in it, so there is nothing here to advance. Skipping has to happen
// where the programming decisions are made, and then this end throws away what
// it has already buffered so the cut is heard now rather than eight seconds
// later. Both halves are required — doing only the first is why SKIP appeared
// to do nothing for minutes at a time.
func (c *Client) SkipChannel(ctx context.Context, channelID string, scope SkipScope) error {
	if channelID == "" {
		return fmt.Errorf("channel id required")
	}
	if scope != SkipKind {
		scope = SkipItem
	}
	return c.post(ctx, "/api/v1/channels/"+url.PathEscape(channelID)+"/skip?scope="+string(scope), nil)
}

// PreviousChannel asks the server to re-air what it played before this.
func (c *Client) PreviousChannel(ctx context.Context, channelID string) error {
	if channelID == "" {
		return fmt.Errorf("channel id required")
	}
	return c.post(ctx, "/api/v1/channels/"+url.PathEscape(channelID)+"/previous", nil)
}

// Verify checks that the stored token still authenticates. Pairing calls it so
// a bad token is reported while someone is looking at the screen.
func (c *Client) Verify(ctx context.Context) error {
	var ignored struct {
		Items []Channel `json:"items"`
	}
	return c.get(ctx, "/api/v1/channels", &ignored)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, out)
}

func (c *Client) post(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodPost, path, out)
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	if !c.Paired() {
		return ErrUnpaired
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("samo %s: %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}
