// Package player is samo-radio's state machine: what the aux port is playing,
// what it falls back to, and how it recovers when a source dies.
package player

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bouliehaan/samo-radio/internal/audio"
	"github.com/bouliehaan/samo-radio/internal/config"
	"github.com/bouliehaan/samo-radio/internal/samo"
	"github.com/bouliehaan/samo-radio/internal/sink"
	"github.com/bouliehaan/samo-radio/internal/source"
)

// ErrNothingToPlay is returned when a command has no target.
var ErrNothingToPlay = errors.New("nothing to play")

// Player owns the sink, the current decoder, and the queue.
type Player struct {
	config *config.Store
	logger *log.Logger

	mu      sync.Mutex
	sink    *sink.Sink
	decoder *source.Decoder
	client  *samo.Client

	mode       Mode
	queue      []Item
	queueIndex int
	current    *Item
	channel    *ChannelState
	paused     bool
	lastError  string
	version    uint64

	// streamID tags the decoder currently attached to the sink. An end-of-
	// stream callback carrying any other id is from a decode that has already
	// been replaced, and must not advance the queue.
	streamID  string
	streamSeq uint64

	// retries counts consecutive failures on the current live source, so a
	// channel that has gone away is retried with a backoff rather than in a
	// tight loop that would spin the CPU for as long as it stays down.
	retries int
	// streamStartedAt is when the attached stream began, which is how the
	// retry counter tells "this channel is down" from "this channel has been
	// fine for six hours and just blipped".
	streamStartedAt time.Time
	// reconnecting means a live source is waiting out a backoff with nothing
	// attached, so status can say so instead of claiming to be playing.
	reconnecting bool

	ctx    context.Context
	cancel context.CancelFunc
	ended  chan string
}

// healthyStreamAfter is how long a live stream must have played before the
// station counts it as having worked. Comfortably past the longest backoff, so
// a run of genuine failures still escalates.
const healthyStreamAfter = 90 * time.Second

// transportCallTimeout bounds a transport button's call back to Samo.
//
// It must stay STRICTLY BELOW the timeout Samo uses when calling this daemon
// (internal/samoradio/client.go, 6s with a 5s response-header timeout). These
// requests nest — the UI asks Samo, Samo asks the device, the device asks Samo
// to skip — and if the outer budget runs out first the UI reports a failure for
// a skip that actually succeeded, which trains you to press it again and skip a
// second item. The inner call has to be the one that gives up first.
const transportCallTimeout = 4 * time.Second

// New builds a player around a config store.
func New(store *config.Store, logger *log.Logger) *Player {
	if logger == nil {
		logger = log.Default()
	}
	snapshot := store.Snapshot()
	return &Player{
		config: store,
		logger: logger,
		mode:   ModeIdle,
		client: samo.New(snapshot.Server.BaseURL, snapshot.Server.Token),
		ended:  make(chan string, 8),
	}
}

// Start opens the sound card and, if configured, tunes the default channel.
func (p *Player) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)

	if err := p.openSink(); err != nil {
		return err
	}
	go p.watchEndings()
	go p.watchChannelMetadata()

	snapshot := p.config.Snapshot()
	if snapshot.AutoTuneOnBoot && snapshot.DefaultStation.Set() && snapshot.Paired() {
		// Boot straight into the fallback station. This is the whole point of
		// the device: after a power cut it should come back on air by itself.
		if err := p.Tune(snapshot.DefaultStation); err != nil {
			p.logger.Printf("auto-tune failed: %v", err)
			p.setError(fmt.Sprintf("auto-tune failed: %v", err))
		}
	}
	return nil
}

// Close stops playback and releases the sound card.
func (p *Player) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopDecodeLocked()
	if p.sink != nil {
		return p.sink.Close()
	}
	return nil
}

// openSink builds a sink from current config and starts it.
func (p *Player) openSink() error {
	snapshot := p.config.Snapshot()
	backend, err := sink.ParseBackend(snapshot.Output.Backend)
	if err != nil {
		return err
	}
	device, err := sink.New(sink.Options{
		Format: audio.Format{
			SampleRate: snapshot.Output.SampleRate,
			Channels:   snapshot.Output.Channels,
		},
		Backend:      backend,
		Device:       snapshot.Output.Device,
		Command:      snapshot.Output.Command,
		BufferMillis: snapshot.Output.BufferMillis,
		Logger:       p.logger,
		OnStreamEnded: func(streamID string) {
			// Called on the pump goroutine, which must never block: a blocked
			// pump is a starved sound card. Hand off and return.
			select {
			case p.ended <- streamID:
			default:
			}
		},
	})
	if err != nil {
		return err
	}
	device.SetVolume(snapshot.Volume)
	if err := device.Start(p.ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.sink = device
	p.mu.Unlock()
	return nil
}

// ----- transport -------------------------------------------------------

// Tune puts the device on a station and leaves it there.
//
// Both kinds are the same thing from here: an endless live source the device
// sits on until something else is asked for. Only the URL differs.
func (p *Player) Tune(station config.Station) error {
	station.ID = strings.TrimSpace(station.ID)
	if station.ID == "" {
		return fmt.Errorf("%w: station id required", ErrNothingToPlay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.client.Paired() {
		return samo.ErrUnpaired
	}
	return p.tuneLocked(station)
}

func (p *Player) tuneLocked(station config.Station) error {
	item, err := p.stationItem(station)
	if err != nil {
		return err
	}
	p.mode = ModeChannel
	p.queue = nil
	p.queueIndex = 0
	p.retries = 0
	p.paused = false
	p.channel = &ChannelState{ID: station.ID, Kind: station.Kind, Name: station.Name}
	p.current = &item
	return p.startCurrentLocked(0)
}

// stationItem builds the playable item for a station. This is the one place
// the daemon constructs a Samo URL itself rather than being handed one — it
// has to, because tuning the fallback happens at boot with nobody to ask.
func (p *Player) stationItem(station config.Station) (Item, error) {
	switch station.Kind {
	case config.StationInternet:
		return Item{
			Ref:       "station:" + station.ID,
			Title:     firstNonEmpty(station.Name, "Internet radio"),
			StreamURL: p.client.InternetStationStreamURL(station.ID),
			Kind:      "station",
			Live:      true,
		}, nil
	case config.StationChannel, "":
		return Item{
			Ref:       "channel:" + station.ID,
			Title:     firstNonEmpty(station.Name, "Channel"),
			StreamURL: p.client.ChannelStreamURL(station.ID),
			Kind:      "channel",
			Live:      true,
		}, nil
	default:
		return Item{}, fmt.Errorf("%w: unknown station kind %q", ErrNothingToPlay, station.Kind)
	}
}

// PlayQueue replaces whatever is on with an ad-hoc list from a client.
func (p *Player) PlayQueue(items []Item, startIndex int) error {
	items = sanitizeItems(items)
	if len(items) == 0 {
		return fmt.Errorf("%w: queue is empty", ErrNothingToPlay)
	}
	if startIndex < 0 || startIndex >= len(items) {
		startIndex = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = ModeQueue
	p.queue = items
	p.queueIndex = startIndex
	p.channel = nil
	p.retries = 0
	p.paused = false
	item := items[startIndex]
	p.current = &item
	return p.startCurrentLocked(0)
}

// Enqueue appends to a running queue, or starts one if the device is on a
// channel or idle. "Play to samo-radio" twice in a row should build a queue,
// not throw away what is playing.
func (p *Player) Enqueue(items []Item) error {
	items = sanitizeItems(items)
	if len(items) == 0 {
		return fmt.Errorf("%w: queue is empty", ErrNothingToPlay)
	}
	p.mu.Lock()
	if p.mode == ModeQueue && len(p.queue) > 0 {
		p.queue = append(p.queue, items...)
		p.bump()
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.PlayQueue(items, 0)
}

// Pause fades out and holds position.
func (p *Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return nil
	}
	p.paused = true
	if p.sink != nil {
		p.sink.SetPaused(true)
	}
	p.bump()
	return nil
}

// Resume fades back in.
//
// A live source is not resumable in the "carry on where you were" sense: the
// broadcast kept going while you were away. Rather than play stale audio out of
// a buffer, it retunes, which is what a radio does when you turn it back on.
func (p *Player) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return ErrNothingToPlay
	}
	p.paused = false
	if p.sink != nil {
		p.sink.SetPaused(false)
	}
	if p.current.Live {
		return p.startCurrentLocked(0)
	}
	p.bump()
	return nil
}

// Next moves on: through the queue if there is one, otherwise through the
// station's own programming.
//
// On a channel this used to refuse outright — "nothing queued" — because the
// device has no items to step through. That reading is too literal to be
// useful: the device is on air twenty-four hours a day and the queue is the
// exceptional state, so for almost all of its life the skip button did nothing
// at all. Whatever is next is a decision the station makes, so ask the station.
func (p *Player) Next() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == ModeChannel {
		return p.skipProgrammeLocked(samo.SkipItem)
	}
	if p.mode != ModeQueue {
		return fmt.Errorf("%w: nothing queued", ErrNothingToPlay)
	}
	return p.advanceLocked(1)
}

// NextKind steps off the whole medium — "not talk right now, put music on".
func (p *Player) NextKind() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != ModeChannel {
		return fmt.Errorf("%w: not on a station", ErrNothingToPlay)
	}
	return p.skipProgrammeLocked(samo.SkipKind)
}

// skipProgrammeLocked asks Samo to move the channel on, then drops everything
// this end has already pulled.
//
// Both halves matter, and the second is the one that makes SKIP feel like a
// button rather than a suggestion. Between the server's encoder and the sound
// card sit its listener queue, a socket, ffmpeg's input buffer and this
// daemon's ring — several seconds of audio that were fetched before the skip
// and would otherwise play out in full afterwards. Restarting the decode throws
// all of it away: the stream is live and endless, so there is nothing to lose
// by reconnecting, and reconnecting is what turns a scheduling decision into an
// audible cut.
//
// An internet station is somebody else's programming and has nothing to skip
// to, so it is refused rather than silently reconnected.
func (p *Player) skipProgrammeLocked(scope samo.SkipScope) error {
	if p.channel == nil || p.channel.Kind == config.StationInternet {
		return fmt.Errorf("%w: an internet station has no programming to skip", ErrNothingToPlay)
	}
	if !p.client.Paired() {
		return samo.ErrUnpaired
	}
	channelID := p.channel.ID
	client := p.client

	// Released around the network call: this is the daemon's only lock and
	// holding it across an HTTP round trip stalls the state endpoint, the SSE
	// stream and every other transport button for as long as the server takes.
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, transportCallTimeout)
	err := client.SkipChannel(ctx, channelID, scope)
	cancel()
	p.mu.Lock()

	if err != nil {
		p.lastError = err.Error()
		p.bump()
		return err
	}
	// Bail if the skip raced something that changed what is on — a retune, a
	// queue arriving from a phone — rather than restarting a stream nobody is
	// on any more.
	if p.mode != ModeChannel || p.channel == nil || p.channel.ID != channelID {
		return nil
	}
	p.retries = 0
	p.paused = false
	return p.startCurrentLocked(0)
}

// Previous steps back, or restarts the current item if it is already past its
// opening seconds — the behaviour every transport control has had since CD
// players, and the one users expect without being told.
func (p *Player) Previous() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == ModeChannel {
		// A live pipe has no back-buffer to rewind into, so going back is also
		// the station's decision: it re-airs the previous item from the top.
		if p.channel == nil || p.channel.Kind == config.StationInternet {
			return fmt.Errorf("%w: an internet station has no programming to rewind", ErrNothingToPlay)
		}
		if !p.client.Paired() {
			return samo.ErrUnpaired
		}
		channelID := p.channel.ID
		client := p.client
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(p.ctx, transportCallTimeout)
		err := client.PreviousChannel(ctx, channelID)
		cancel()
		p.mu.Lock()
		if err != nil {
			p.lastError = err.Error()
			p.bump()
			return err
		}
		if p.mode != ModeChannel || p.channel == nil || p.channel.ID != channelID {
			return nil
		}
		p.retries = 0
		p.paused = false
		return p.startCurrentLocked(0)
	}
	if p.mode != ModeQueue {
		return fmt.Errorf("%w: nothing queued", ErrNothingToPlay)
	}
	if p.sink != nil && p.sink.PositionSeconds() > 3 {
		return p.startCurrentLocked(0)
	}
	return p.advanceLocked(-1)
}

// Seek jumps within the current item by restarting the decode at an offset.
// ffmpeg's input seek makes this cheap, and it keeps the sink's byte-counting
// position honest: there is exactly one clock and it is the sound card.
func (p *Player) Seek(seconds float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return ErrNothingToPlay
	}
	if p.current.Live {
		return errors.New("cannot seek a live source")
	}
	if seconds < 0 {
		seconds = 0
	}
	if p.current.DurationSeconds > 0 && seconds > p.current.DurationSeconds {
		seconds = p.current.DurationSeconds
	}
	p.paused = false
	return p.startCurrentLocked(seconds)
}

// Stop ends the ad-hoc session and hands the aux port back to the fallback
// channel. On a device whose job is to always be on air, "stop" means "stop
// what I asked for", not "go silent".
func (p *Player) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fallbackLocked()
}

// Standby is the real off switch: sink open, output silent.
func (p *Player) Standby() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopDecodeLocked()
	p.mode = ModeIdle
	p.queue = nil
	p.queueIndex = 0
	p.current = nil
	p.channel = nil
	p.paused = false
	if p.sink != nil {
		p.sink.ClearStream()
	}
	p.bump()
	return nil
}

// SetVolume sets and persists the output level.
func (p *Player) SetVolume(volume float64) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 1 {
		volume = 1
	}
	if _, err := p.config.Update(func(c *config.Config) error {
		c.Volume = volume
		return nil
	}); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink != nil {
		p.sink.SetVolume(volume)
	}
	p.bump()
	return nil
}

// ----- configuration ---------------------------------------------------

// Pair stores server credentials and re-points the client at them.
func (p *Player) Pair(ctx context.Context, baseURL, token, serverName, deviceName string) error {
	client := samo.New(baseURL, token)
	if !client.Paired() {
		return errors.New("server url and token are both required")
	}
	// Fail here, while somebody is looking at the pairing screen, rather than
	// silently at 3am when the device tries to tune itself.
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Verify(verifyCtx); err != nil {
		return fmt.Errorf("could not authenticate with %s: %w", baseURL, err)
	}
	if _, err := p.config.Update(func(c *config.Config) error {
		c.Server.BaseURL = baseURL
		c.Server.Token = token
		c.Server.ServerName = strings.TrimSpace(serverName)
		if name := strings.TrimSpace(deviceName); name != "" {
			c.DeviceName = name
		}
		return nil
	}); err != nil {
		return err
	}
	p.mu.Lock()
	p.client = client
	p.lastError = ""
	p.bump()
	p.mu.Unlock()
	return nil
}

// SetDefaultStation changes what the device falls back to.
func (p *Player) SetDefaultStation(station config.Station, tuneNow bool) error {
	if _, err := p.config.Update(func(c *config.Config) error {
		c.DefaultStation = station
		return nil
	}); err != nil {
		return err
	}
	if tuneNow && strings.TrimSpace(station.ID) != "" {
		return p.Tune(station)
	}
	p.mu.Lock()
	p.bump()
	p.mu.Unlock()
	return nil
}

// SetAutoTune toggles tuning the fallback station at boot.
func (p *Player) SetAutoTune(enabled bool) error {
	_, err := p.config.Update(func(c *config.Config) error {
		c.AutoTuneOnBoot = enabled
		return nil
	})
	p.mu.Lock()
	p.bump()
	p.mu.Unlock()
	return err
}

// SetOutput changes the sound card and reopens the device without a restart,
// resuming whatever was playing. Picking the right socket is a trial-and-error
// job — making the operator restart a service between attempts, over SSH, is
// the exact experience this whole project exists to avoid.
func (p *Player) SetOutput(backend, device string, sampleRate, channels, bufferMillis int, command []string) error {
	parsed, err := sink.ParseBackend(backend)
	if err != nil {
		return err
	}
	if _, err := p.config.Update(func(c *config.Config) error {
		c.Output.Backend = string(parsed)
		c.Output.Device = strings.TrimSpace(device)
		if sampleRate > 0 {
			c.Output.SampleRate = sampleRate
		}
		if channels > 0 {
			c.Output.Channels = channels
		}
		if bufferMillis > 0 {
			c.Output.BufferMillis = bufferMillis
		}
		if command != nil {
			c.Output.Command = command
		}
		return nil
	}); err != nil {
		return err
	}
	return p.reopenSink()
}

// reopenSink swaps the sound card underneath live playback.
func (p *Player) reopenSink() error {
	p.mu.Lock()
	previous := p.sink
	position := 0.0
	if previous != nil {
		position = previous.PositionSeconds()
	}
	resume := p.current
	p.stopDecodeLocked()
	p.sink = nil
	p.mu.Unlock()

	if previous != nil {
		_ = previous.Close()
	}
	if err := p.openSink(); err != nil {
		p.setError(fmt.Sprintf("output change failed: %v", err))
		return err
	}
	if resume == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if resume.Live {
		position = 0
	}
	return p.startCurrentLocked(position)
}

// ----- internals -------------------------------------------------------

// startCurrentLocked spawns a decoder for p.current and attaches it.
func (p *Player) startCurrentLocked(startSeconds float64) error {
	if p.current == nil {
		return ErrNothingToPlay
	}
	if p.sink == nil {
		return errors.New("audio output is not open")
	}
	p.stopDecodeLocked()

	snapshot := p.config.Snapshot()
	decoder, err := source.Start(p.ctx, source.Options{
		URL:            p.current.StreamURL,
		Headers:        p.authHeadersFor(p.current.StreamURL),
		StartSeconds:   startSeconds,
		Live:           p.current.Live,
		GainDB:         p.current.GainDB,
		LimitPeaks:     p.current.LimitPeaks,
		CeilingDBTP:    p.current.CeilingDBTP,
		Format:         p.sink.Format(),
		FFmpegPath:     snapshot.FFmpegPath,
		BufferSeconds:  snapshot.BufferSeconds,
		PrefillSeconds: snapshot.PrefillSeconds,
		Logger:         p.logger,
	})
	if err != nil {
		p.lastError = err.Error()
		p.bump()
		return err
	}
	p.streamSeq++
	p.streamID = fmt.Sprintf("stream-%d", p.streamSeq)
	p.decoder = decoder
	p.lastError = ""
	p.streamStartedAt = time.Now()
	p.sink.SetStream(&sink.Stream{ID: p.streamID, Ring: decoder.Ring(), StartSeconds: startSeconds})
	p.sink.SetPaused(p.paused)
	p.bump()
	return nil
}

// authHeadersFor only attaches the Samo token to Samo's own URLs, so a
// live-stream source pointing at some third-party icecast server never leaks
// the credential off-host.
func (p *Player) authHeadersFor(streamURL string) map[string]string {
	base := p.client.BaseURL()
	if base == "" || !strings.HasPrefix(streamURL, base) {
		return nil
	}
	return p.client.AuthHeaders()
}

func (p *Player) stopDecodeLocked() {
	if p.decoder != nil {
		p.decoder.Close()
		p.decoder = nil
	}
	p.streamID = ""
}

// advanceLocked moves within the queue, falling back when it runs out.
func (p *Player) advanceLocked(delta int) error {
	next := p.queueIndex + delta
	if next < 0 {
		next = 0
	}
	if next >= len(p.queue) {
		return p.fallbackLocked()
	}
	p.queueIndex = next
	item := p.queue[next]
	p.current = &item
	p.paused = false
	p.retries = 0
	return p.startCurrentLocked(0)
}

// fallbackLocked returns the device to its resting state.
func (p *Player) fallbackLocked() error {
	snapshot := p.config.Snapshot()
	p.stopDecodeLocked()
	p.queue = nil
	p.queueIndex = 0
	p.paused = false

	if !snapshot.DefaultStation.Set() || !p.client.Paired() {
		p.mode = ModeIdle
		p.current = nil
		p.channel = nil
		if p.sink != nil {
			p.sink.ClearStream()
		}
		p.bump()
		return nil
	}
	return p.tuneLocked(snapshot.DefaultStation)
}

// watchEndings turns "the sink drained a stream" into a queue decision.
func (p *Player) watchEndings() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case streamID := <-p.ended:
			p.handleEnded(streamID)
		}
	}
}

func (p *Player) handleEnded(streamID string) {
	p.mu.Lock()
	// A stream that was already replaced (skip, seek, retune) is allowed to
	// finish quietly; only the attached one drives the queue.
	if streamID == "" || streamID != p.streamID {
		p.mu.Unlock()
		return
	}
	failure := ""
	if p.decoder != nil {
		failure = p.decoder.Err()
	}
	p.stopDecodeLocked()

	if failure != "" {
		p.logger.Printf("source ended with error: %s", failure)
		p.lastError = failure
	}

	// A live source is never "finished" — if its pipe closed, either the
	// network dropped or the server restarted. Retry with a backoff and stay on
	// air; going silent because a channel hiccuped is the wrong answer for a
	// device nobody is standing next to.
	if p.current != nil && p.current.Live {
		// A stream that stayed up for a while was healthy, so this is a NEW
		// fault rather than a continuation of an old one.
		//
		// The counter is meant to be CONSECUTIVE failures, but nothing was ever
		// resetting it on a stream that came back and played — every reset in
		// this file is on an explicit user action (tune, skip, back). The device
		// tunes once at boot and stays there for weeks, so the count was really
		// "hiccups since the last reboot": after six unrelated blips, spread
		// over days, every reconnection waited the full thirty seconds.
		if !p.streamStartedAt.IsZero() && time.Since(p.streamStartedAt) >= healthyStreamAfter {
			p.retries = 0
		}
		item := *p.current
		p.reconnecting = true
		p.logger.Printf("live source ended (%s); reconnecting", firstNonEmpty(failure, "eof"))
		p.bump()

		// Keep trying. Giving up after ONE failed restart left the daemon with
		// no decoder attached — and the only thing that produces an end-of-
		// stream event is a decoder, so nothing would ever retry again. A
		// momentary failure to spawn ffmpeg took the aux port off the air until
		// somebody noticed and restarted the service.
		for {
			p.retries++
			delay := backoff(p.retries)
			p.mu.Unlock()
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(delay):
			}
			p.mu.Lock()
			// Bail if anything changed while waiting.
			if p.current == nil || p.current.Ref != item.Ref {
				p.reconnecting = false
				p.mu.Unlock()
				return
			}
			err := p.startCurrentLocked(0)
			if err == nil {
				p.reconnecting = false
				p.mu.Unlock()
				return
			}
			p.logger.Printf("live source retry failed: %v", err)
			p.lastError = err.Error()
		}
	}

	if p.mode == ModeQueue {
		if err := p.advanceLocked(1); err != nil && !errors.Is(err, ErrNothingToPlay) {
			p.logger.Printf("advance failed: %v", err)
		}
		p.mu.Unlock()
		return
	}
	if err := p.fallbackLocked(); err != nil {
		p.logger.Printf("fallback failed: %v", err)
	}
	p.mu.Unlock()
}

// watchChannelMetadata keeps "what's on" current while tuned to a channel. The
// channel stream is a bare MP3 pipe with no in-band metadata, so the only way
// to know what is airing is to ask the server.
func (p *Player) watchChannelMetadata() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.refreshChannelMetadata()
		}
	}
}

func (p *Player) refreshChannelMetadata() {
	p.mu.Lock()
	if p.mode != ModeChannel || p.channel == nil {
		p.mu.Unlock()
		return
	}
	tuned := *p.channel
	client := p.client
	p.mu.Unlock()

	if !client.Paired() {
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 8*time.Second)
	defer cancel()

	next := tuned
	switch tuned.Kind {
	case config.StationInternet:
		// Samo probes internet stations for ICY metadata on its own schedule,
		// so asking it beats parsing the stream here.
		station, err := client.InternetStationByID(ctx, tuned.ID)
		if err != nil {
			return
		}
		if strings.TrimSpace(station.Name) != "" {
			next.Name = station.Name
		}
		next.Title, next.Artist = "", ""
		if station.NowPlaying != nil {
			next.Title = firstNonEmpty(station.NowPlaying.Title, station.NowPlaying.Raw)
			next.Artist = station.NowPlaying.Artist
		}
	default:
		now, err := client.NowPlaying(ctx, tuned.ID)
		if err != nil {
			return
		}
		next.ListenerCount = now.ListenerCount
		if now.Current != nil {
			next.Title = now.Current.Title
			next.Artist = now.Current.Artist
			next.SourceLabel = now.Current.SourceLabel
		} else {
			next.Title, next.Artist, next.SourceLabel = "", "", ""
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != ModeChannel || p.channel == nil || p.channel.ID != tuned.ID {
		return
	}
	if next != *p.channel {
		p.channel = &next
		p.bump()
	}
}

// ----- state -----------------------------------------------------------

// State returns a full snapshot.
func (p *Player) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := p.config.Snapshot()

	state := State{
		DeviceName: snapshot.DeviceName,
		Mode:       p.mode,
		Volume:     snapshot.Volume,
		QueueIndex: p.queueIndex,

		Error:     p.lastError,
		UpdatedAt: time.Now().UTC(),
		Version:   p.version,
		Server: ServerState{
			BaseURL: snapshot.Server.BaseURL,
			Name:    snapshot.Server.ServerName,
			Paired:  snapshot.Paired(),
		},
		Output: OutputState{
			Backend:    snapshot.Output.Backend,
			Device:     snapshot.Output.Device,
			SampleRate: snapshot.Output.SampleRate,
			Channels:   snapshot.Output.Channels,
		},
	}
	if snapshot.DefaultStation.Set() {
		state.DefaultStation = &StationRef{
			Kind: snapshot.DefaultStation.Kind,
			ID:   snapshot.DefaultStation.ID,
			Name: snapshot.DefaultStation.Name,
		}
	}
	if p.sink != nil {
		state.Output.Backend = string(p.sink.Backend())
		state.Output.Open = p.sink.Running()
		state.Output.Restarts = p.sink.Restarts()
		state.Output.LastError = p.sink.LastError()
		state.PositionSeconds = p.sink.PositionSeconds()
	}
	if p.current != nil {
		item := *p.current
		state.Item = &item
		state.DurationSeconds = item.DurationSeconds
	}
	if p.channel != nil {
		channel := *p.channel
		state.Channel = &channel
	}
	if len(p.queue) > 0 {
		state.Queue = append([]Item(nil), p.queue...)
	}
	state.Status = p.statusLocked()
	return state
}

func (p *Player) statusLocked() Status {
	switch {
	case p.current == nil:
		return StatusIdle
	case p.paused:
		return StatusPaused
	case p.reconnecting:
		// Waiting out a backoff with no decoder attached. Reporting "playing"
		// here meant the RADIO panel could not tell music from dead air.
		return StatusBuffering
	case p.sink != nil && p.sink.Rebuffering():
		return StatusBuffering
	case p.sink != nil && !p.sink.Running():
		return StatusError
	default:
		return StatusPlaying
	}
}

// Client exposes the Samo client for handlers that proxy channel lookups.
func (p *Player) Client() *samo.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

func (p *Player) setError(message string) {
	p.mu.Lock()
	p.lastError = message
	p.bump()
	p.mu.Unlock()
}

// bump marks a structural change. Callers must hold the lock.
func (p *Player) bump() { p.version++ }

func sanitizeItems(items []Item) []Item {
	cleaned := make([]Item, 0, len(items))
	for _, item := range items {
		item.StreamURL = strings.TrimSpace(item.StreamURL)
		if item.StreamURL == "" {
			continue
		}
		item.Title = strings.TrimSpace(item.Title)
		if item.Title == "" {
			item.Title = "Unknown"
		}
		if strings.TrimSpace(item.Ref) == "" {
			item.Ref = item.StreamURL
		}
		cleaned = append(cleaned, item)
	}
	return cleaned
}

// backoff grows 1s, 2s, 4s… capped at 30s.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Second << (attempt - 1)
	if delay > 30*time.Second || delay <= 0 {
		delay = 30 * time.Second
	}
	return delay
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
