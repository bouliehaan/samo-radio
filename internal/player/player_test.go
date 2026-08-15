package player

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bouliehaan/samo-radio/internal/config"
	"github.com/bouliehaan/samo-radio/internal/samo"
)

func newTestPlayer(t *testing.T, baseURL string) *Player {
	t.Helper()
	store, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := New(store, nil)
	p.client = samo.New(baseURL, "secret-token")
	return p
}

// The device's Samo token must never ride along to a host that is not Samo. A
// channel can legitimately contain a third-party internet station, and the
// stream URL for one points straight at somebody else's icecast server.
func TestAuthHeadersOnlyGoToSamo(t *testing.T) {
	p := newTestPlayer(t, "https://samo.example.com")

	headers := p.authHeadersFor("https://samo.example.com/channels/abc/stream")
	if headers["Authorization"] != "Bearer secret-token" {
		t.Fatalf("expected the Samo token on a Samo URL, got %v", headers)
	}

	if headers := p.authHeadersFor("https://someone-elses-icecast.example.net/live.mp3"); headers != nil {
		t.Fatalf("token leaked to a third-party host: %v", headers)
	}
}

func TestAuthHeadersWithoutPairing(t *testing.T) {
	p := newTestPlayer(t, "")
	if headers := p.authHeadersFor("https://samo.example.com/x"); headers != nil {
		t.Fatalf("an unpaired device has no credentials to send, got %v", headers)
	}
}

func TestSanitizeItemsDropsUnplayableAndFillsDefaults(t *testing.T) {
	items := sanitizeItems([]Item{
		{Title: "no url"},
		{StreamURL: "  https://samo.example.com/a  ", Title: "  Song  "},
		{StreamURL: "https://samo.example.com/b"},
	})
	if len(items) != 2 {
		t.Fatalf("expected the url-less item to be dropped, got %d items", len(items))
	}
	if items[0].StreamURL != "https://samo.example.com/a" || items[0].Title != "Song" {
		t.Fatalf("expected trimmed fields, got %+v", items[0])
	}
	if items[1].Title != "Unknown" {
		t.Fatalf("expected a placeholder title, got %q", items[1].Title)
	}
	// A missing ref falls back to the URL so the retry path can still tell
	// "the item I was playing" from "the item that replaced it".
	if items[1].Ref != "https://samo.example.com/b" {
		t.Fatalf("expected the URL as the fallback ref, got %q", items[1].Ref)
	}
}

func TestPlayQueueRejectsEmpty(t *testing.T) {
	p := newTestPlayer(t, "https://samo.example.com")
	if err := p.PlayQueue(nil, 0); err == nil {
		t.Fatal("expected an empty queue to be rejected")
	}
	if err := p.PlayQueue([]Item{{Title: "no url"}}, 0); err == nil {
		t.Fatal("expected a queue of unplayable items to be rejected")
	}
}

func TestTuneRequiresPairing(t *testing.T) {
	p := newTestPlayer(t, "")
	station := config.Station{Kind: config.StationChannel, ID: "channel_1", Name: "Drive Home"}
	if err := p.Tune(station); err == nil {
		t.Fatal("expected an unpaired device to refuse to tune")
	}
}

// The fallback is what makes this a radio, and it has to work for an internet
// station as well as a channel — the URL is the only thing that differs.
func TestStationItemBuildsBothKinds(t *testing.T) {
	p := newTestPlayer(t, "https://samo.example.com")

	channel, err := p.stationItem(config.Station{Kind: config.StationChannel, ID: "ch_1", Name: "Drive Home"})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if channel.StreamURL != "https://samo.example.com/channels/ch_1/stream" {
		t.Fatalf("unexpected channel URL %q", channel.StreamURL)
	}

	station, err := p.stationItem(config.Station{Kind: config.StationInternet, ID: "st_1", Name: "Diamond City Radio"})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	if station.StreamURL != "https://samo.example.com/internet-radio/st_1/stream" {
		t.Fatalf("unexpected station URL %q", station.StreamURL)
	}
	if station.Title != "Diamond City Radio" || station.Ref != "station:st_1" {
		t.Fatalf("unexpected station item %+v", station)
	}
	// Both are endless: no seeking, and a dropped connection is retried rather
	// than treated as the end of the item.
	if !channel.Live || !station.Live {
		t.Fatal("stations of both kinds must be marked live")
	}

	if _, err := p.stationItem(config.Station{Kind: "nonsense", ID: "x"}); err == nil {
		t.Fatal("an unknown station kind must be rejected, not silently played as a channel")
	}
}

// A device configured before internet stations existed must keep its fallback
// across the upgrade rather than coming back silent.
func TestLegacyDefaultChannelMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"defaultChannelId":"ch_9","defaultChannelName":"Old Faithful"}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	store, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	station := store.Snapshot().DefaultStation
	if station.ID != "ch_9" || station.Kind != config.StationChannel || station.Name != "Old Faithful" {
		t.Fatalf("legacy default channel did not migrate: %+v", station)
	}
}

// Backoff has to actually grow and then stop growing: a channel that is down
// for a week must not spin, and must not wander off to hour-long retries.
func TestBackoffGrowsAndCaps(t *testing.T) {
	if got := backoff(1); got != time.Second {
		t.Fatalf("first retry: got %v", got)
	}
	if got := backoff(3); got != 4*time.Second {
		t.Fatalf("third retry: got %v", got)
	}
	for _, attempt := range []int{7, 20, 200} {
		if got := backoff(attempt); got != 30*time.Second {
			t.Fatalf("attempt %d should cap at 30s, got %v", attempt, got)
		}
	}
}

// An unpaired device with no fallback channel goes quiet rather than erroring
// in a loop — there is genuinely nothing for it to play.
func TestFallbackWithoutDefaultStationGoesIdle(t *testing.T) {
	p := newTestPlayer(t, "")
	p.mu.Lock()
	err := p.fallbackLocked()
	mode := p.mode
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if mode != ModeIdle {
		t.Fatalf("expected idle, got %s", mode)
	}
}

// Pairing stores nothing it has not proved: the credentials have to work now,
// while somebody is looking at the pairing screen.
func TestPairStoresTheServerItCanReach(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer device-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()

	p := newTestPlayer(t, "")
	if err := p.Pair(context.Background(), PairRequest{
		ServerURL:  server.URL,
		Token:      "device-token",
		DeviceName: "Kitchen",
		CallerHost: "127.0.0.1",
	}); err != nil {
		t.Fatalf("pair: %v", err)
	}

	snapshot := p.config.Snapshot()
	if !snapshot.Paired() || snapshot.Server.BaseURL != server.URL {
		t.Fatalf("pairing was not stored: %+v", snapshot.Server)
	}
	if snapshot.DeviceName != "Kitchen" {
		t.Fatalf("device name was not taken from the pairing: %q", snapshot.DeviceName)
	}
}

// A device that cannot reach Samo must stay unpaired rather than store a URL
// it will spend the next week failing to fetch audio from — and must say why
// while there is still somebody to read it.
func TestPairRejectsAServerItCannotReach(t *testing.T) {
	p := newTestPlayer(t, "")
	// Port 1 is nothing, on any machine: connection refused, immediately.
	err := p.Pair(context.Background(), PairRequest{ServerURL: "http://127.0.0.1:1", Token: "device-token"})
	if err == nil {
		t.Fatal("pairing with an unreachable server must fail")
	}
	if !strings.Contains(err.Error(), "another machine") {
		t.Fatalf("the error should point at the likeliest cause, got: %v", err)
	}
	if p.config.Snapshot().Paired() {
		t.Fatal("a failed pairing must not be stored")
	}
}

func TestStateReportsUnpairedDevice(t *testing.T) {
	p := newTestPlayer(t, "")
	state := p.State()
	if state.Server.Paired {
		t.Fatal("expected paired=false")
	}
	if state.Status != StatusIdle {
		t.Fatalf("expected idle status, got %s", state.Status)
	}
	if state.Volume <= 0 {
		t.Fatalf("expected a default volume, got %v", state.Volume)
	}
}
