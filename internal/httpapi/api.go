// Package httpapi is samo-radio's control surface.
//
// It is not a public API. samo-server is the only thing expected to call it —
// phones and browsers reach the device *through* Samo, which already has
// accounts, tokens and a tunnel, and this daemon has no business growing a
// second version of any of that.
//
// What it cannot assume is that Samo is on the same machine. The device may be
// a Pi in another room, so the API is reachable across the LAN and the control
// token is the whole of the protection: every route below except health
// requires it, and a device with no token configured serves nothing rather than
// serving everyone.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bouliehaan/samo-radio/internal/config"
	"github.com/bouliehaan/samo-radio/internal/player"
	"github.com/bouliehaan/samo-radio/internal/samo"
	"github.com/bouliehaan/samo-radio/internal/sink"
)

// Version is reported by /v1/health so the server can tell what it is talking
// to without a separate handshake.
const Version = "1.0.0"

// Handler serves the control API.
type Handler struct {
	player *player.Player
	config *config.Store
	logger *log.Logger
	mux    *http.ServeMux
}

// New wires the routes.
func New(p *player.Player, store *config.Store, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Default()
	}
	h := &Handler{player: p, config: store, logger: logger, mux: http.NewServeMux()}
	h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) routes() {
	// Health is unauthenticated so systemd, a watchdog or a curl from the
	// console can answer "is it alive" without credentials. It deliberately
	// leaks nothing beyond liveness and pairing status.
	h.mux.HandleFunc("GET /v1/health", h.health)

	h.mux.HandleFunc("GET /v1/state", h.guard(h.state))
	h.mux.HandleFunc("GET /v1/events", h.guard(h.events))
	h.mux.HandleFunc("GET /v1/outputs", h.guard(h.outputs))
	h.mux.HandleFunc("GET /v1/channels", h.guard(h.channels))

	h.mux.HandleFunc("POST /v1/pair", h.guard(h.pair))
	h.mux.HandleFunc("POST /v1/play", h.guard(h.play))
	h.mux.HandleFunc("POST /v1/enqueue", h.guard(h.enqueue))
	h.mux.HandleFunc("POST /v1/pause", h.guard(h.simple(func() error { return h.player.Pause() })))
	h.mux.HandleFunc("POST /v1/resume", h.guard(h.simple(func() error { return h.player.Resume() })))
	h.mux.HandleFunc("POST /v1/next", h.guard(h.simple(func() error { return h.player.Next() })))
	// On a channel, "next kind" steps off the whole medium rather than the one
	// item — the difference between "not this episode" and "not talk radio".
	h.mux.HandleFunc("POST /v1/next-kind", h.guard(h.simple(func() error { return h.player.NextKind() })))
	h.mux.HandleFunc("POST /v1/previous", h.guard(h.simple(func() error { return h.player.Previous() })))
	h.mux.HandleFunc("POST /v1/stop", h.guard(h.simple(func() error { return h.player.Stop() })))
	h.mux.HandleFunc("POST /v1/standby", h.guard(h.simple(func() error { return h.player.Standby() })))
	h.mux.HandleFunc("POST /v1/seek", h.guard(h.seek))
	h.mux.HandleFunc("POST /v1/volume", h.guard(h.volume))
	h.mux.HandleFunc("PATCH /v1/settings", h.guard(h.settings))
}

// guard enforces the shared control secret, and fails closed without one.
//
// This used to wave requests through when no token was configured, which was
// defensible while the API only ever answered on loopback. It is not defensible
// on a device that ships listening to the LAN: an empty token would hand the
// speakers to anything on the network. The daemon mints one at startup, so an
// empty token here means somebody blanked it by hand, and refusing is both
// safer and easier to diagnose than silently obeying strangers.
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(h.config.Snapshot().ControlToken)
		if expected == "" {
			writeError(w, http.StatusServiceUnavailable,
				"no control token configured — run samo-radio --pairing on the device to mint one")
			return
		}
		provided := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid control token")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header != "" {
		if after, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return header
	}
	return strings.TrimSpace(r.Header.Get("X-Samo-Radio-Token"))
}

// ----- handlers --------------------------------------------------------

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	snapshot := h.config.Snapshot()
	state := h.player.State()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"service":    "samo-radio",
		"version":    Version,
		"deviceName": snapshot.DeviceName,
		"paired":     snapshot.Paired(),
		"outputOpen": state.Output.Open,
	})
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.player.State())
}

// events streams the same snapshot the state endpoint returns, once a second.
//
// A full snapshot per frame rather than a delta: position moves every second
// anyway, so there is no bandwidth to save by being clever, and a subscriber
// that misses frames still converges on the truth.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	send := func() bool {
		payload, err := json.Marshal(h.player.State())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func (h *Handler) outputs(w http.ResponseWriter, r *http.Request) {
	snapshot := h.config.Snapshot()
	requested := r.URL.Query().Get("backend")
	if strings.TrimSpace(requested) == "" {
		requested = snapshot.Output.Backend
	}
	backend, err := sink.ParseBackend(requested)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	devices, resolved, err := sink.ListDevices(r.Context(), backend)
	if err != nil {
		// A failure to enumerate is not fatal — it usually means the tool is
		// missing. Report it inline so the UI can say why the list is empty
		// instead of showing a blank panel.
		writeJSON(w, http.StatusOK, map[string]any{
			"backend":  string(resolved),
			"devices":  []sink.Device{},
			"selected": snapshot.Output.Device,
			"error":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":  string(resolved),
		"devices":  devices,
		"selected": snapshot.Output.Device,
		"backends": sink.KnownBackends(),
	})
}

// channels proxies the server's channel list so a client talking to the device
// can offer "tune to…" without needing its own Samo credentials.
func (h *Handler) channels(w http.ResponseWriter, r *http.Request) {
	client := h.player.Client()
	items, err := client.Channels(r.Context())
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, samo.ErrUnpaired) {
			status = http.StatusPreconditionRequired
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

type pairRequest struct {
	ServerURL  string `json:"serverUrl"`
	Token      string `json:"token"`
	ServerName string `json:"serverName,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

func (h *Handler) pair(w http.ResponseWriter, r *http.Request) {
	var body pairRequest
	if !decode(w, r, &body) {
		return
	}
	if err := h.player.Pair(r.Context(), player.PairRequest{
		ServerURL:  body.ServerURL,
		Token:      body.Token,
		ServerName: body.ServerName,
		DeviceName: body.DeviceName,
		// Where the request came from, which is how the device recovers when
		// Samo tells it to fetch audio from a loopback address that means
		// nothing here. See player.PairRequest.CallerHost.
		CallerHost: callerHost(r),
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.player.State())
}

// callerHost is the peer address of a request, with the noise stripped.
//
// Only the TCP peer is trusted: X-Forwarded-For is a header anyone can write,
// and this value is used to decide where the device fetches audio from.
func callerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	host = strings.Trim(host, "[]")
	// An IPv6 zone ("fe80::1%eth0") is meaningful to this host and to nobody
	// else, and would not survive being pasted into a URL.
	host, _, _ = strings.Cut(host, "%")
	// A server calling from an IPv4-mapped address (::ffff:192.168.1.10) is on
	// the LAN at the plain address; the mapped form is not a URL host.
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return host
}

type playRequest struct {
	Mode        string        `json:"mode"`
	ChannelID   string        `json:"channelId,omitempty"`
	ChannelName string        `json:"channelName,omitempty"`
	StationID   string        `json:"stationId,omitempty"`
	StationName string        `json:"stationName,omitempty"`
	Items       []player.Item `json:"items,omitempty"`
	StartIndex  int           `json:"startIndex,omitempty"`
}

func (h *Handler) play(w http.ResponseWriter, r *http.Request) {
	var body playRequest
	if !decode(w, r, &body) {
		return
	}
	var err error
	switch strings.ToLower(strings.TrimSpace(body.Mode)) {
	case "channel":
		err = h.player.Tune(config.Station{
			Kind: config.StationChannel,
			ID:   body.ChannelID,
			Name: body.ChannelName,
		})
	case "station":
		err = h.player.Tune(config.Station{
			Kind: config.StationInternet,
			ID:   body.StationID,
			Name: body.StationName,
		})
	case "queue", "items", "":
		err = h.player.PlayQueue(body.Items, body.StartIndex)
	default:
		writeError(w, http.StatusBadRequest, "unknown play mode "+body.Mode)
		return
	}
	h.respond(w, err)
}

func (h *Handler) enqueue(w http.ResponseWriter, r *http.Request) {
	var body playRequest
	if !decode(w, r, &body) {
		return
	}
	h.respond(w, h.player.Enqueue(body.Items))
}

func (h *Handler) seek(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PositionSeconds float64 `json:"positionSeconds"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.respond(w, h.player.Seek(body.PositionSeconds))
}

func (h *Handler) volume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Volume float64 `json:"volume"`
	}
	if !decode(w, r, &body) {
		return
	}
	h.respond(w, h.player.SetVolume(body.Volume))
}

type settingsRequest struct {
	DeviceName     *string         `json:"deviceName,omitempty"`
	DefaultStation *config.Station `json:"defaultStation,omitempty"`
	TuneNow        bool            `json:"tuneNow,omitempty"`
	AutoTuneOnBoot *bool           `json:"autoTuneOnBoot,omitempty"`

	Output *struct {
		Backend      string   `json:"backend,omitempty"`
		Device       *string  `json:"device,omitempty"`
		SampleRate   int      `json:"sampleRate,omitempty"`
		Channels     int      `json:"channels,omitempty"`
		BufferMillis int      `json:"bufferMillis,omitempty"`
		Command      []string `json:"command,omitempty"`
	} `json:"output,omitempty"`
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	var body settingsRequest
	if !decode(w, r, &body) {
		return
	}

	if body.DeviceName != nil {
		if _, err := h.config.Update(func(c *config.Config) error {
			c.DeviceName = strings.TrimSpace(*body.DeviceName)
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.AutoTuneOnBoot != nil {
		if err := h.player.SetAutoTune(*body.AutoTuneOnBoot); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.DefaultStation != nil {
		if err := h.player.SetDefaultStation(*body.DefaultStation, body.TuneNow); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Output != nil {
		snapshot := h.config.Snapshot()
		backend := body.Output.Backend
		if strings.TrimSpace(backend) == "" {
			backend = snapshot.Output.Backend
		}
		device := snapshot.Output.Device
		if body.Output.Device != nil {
			device = *body.Output.Device
		}
		if err := h.player.SetOutput(
			backend,
			device,
			body.Output.SampleRate,
			body.Output.Channels,
			body.Output.BufferMillis,
			body.Output.Command,
		); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, h.player.State())
}

// simple adapts a no-argument transport command to a handler.
func (h *Handler) simple(action func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.respond(w, action())
	}
}

// respond turns a command result into a status plus the new state, so a client
// never has to follow a command with a state fetch.
func (h *Handler) respond(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, h.player.State())
	case errors.Is(err, samo.ErrUnpaired):
		writeError(w, http.StatusPreconditionRequired, err.Error())
	case errors.Is(err, player.ErrNothingToPlay):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ----- plumbing --------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// Serve runs the control API until the context is cancelled.
func Serve(ctx context.Context, addr string, handler http.Handler, logger *log.Logger) error {
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
		// No write timeout: /v1/events is a long-lived SSE stream and any
		// deadline here would sever it on a timer.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Printf("control api listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
