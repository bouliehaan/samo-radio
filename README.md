# samo-radio

A headless Samo playback device. It runs on the same machine as
[samo-server](../samo-server), holds the sound card open, and plays whatever
Samo tells it to — out the box's own line-out, into whatever is plugged in.

Two things in one:

- **A station.** It tunes a Samo channel *or* an internet radio station and
  stays there. On boot, after a power cut, after a network drop, it comes back
  on air by itself. Nobody has to log in.
- **A cast target.** From the phone or the web UI, "play to samo-radio" sends a
  queue to it — the aux port picks it up, and when the queue runs out it drops
  back to the station.

## How it fits together

```
  phone / web UI                samo-server                 samo-radio
        │                            │                          │
        │  POST /api/v1/samo-radio/  │                          │
        │  devices/{id}/play         │  POST /v1/play           │
        ├───────────────────────────►├─────────────────────────►│
        │                            │  (resolved stream URLs)  │
        │                            │                          ▼
        │                            │◄───── GET /channels/…/stream
        │                            │       GET /api/v1/…/stream
        │                            │           (Bearer token) │
        │                            │                          ▼
        │                            │                    ffmpeg → PCM
        │                            │                          ▼
        │                            │                  ┌───────────────┐
        │                            │                  │ always-open   │
        │                            │                  │ sink (aplay)  │──► 🔊 aux
        │                            │                  └───────────────┘
```

Clients never talk to this daemon directly. samo-server proxies every command,
so control works from anywhere Samo works — including through a tunnel — and
this process never has to grow its own accounts, sessions or TLS.

## Design notes

**The sound card is opened once and never closed.** When there is nothing to
play, the sink is fed digital silence instead of being released. That costs a
few hundred kB/s of zeroes and buys: no reopen race with anything else that
might grab the card, no relay click or DC pop between tracks, and no "device or
resource busy" that only shows up when you are not in the room.

**The sink is the clock.** Writing to the card blocks once its buffer is full,
so the pump loop — read a frame, write a frame — self-paces at exactly real
time. There are no timers in the audio path.

**A ring buffer sits between ffmpeg and the sink.** Writes into it block
(backpressure, so a decoder cannot buffer an album into memory); reads never
block (an empty ring is a silent frame, not a stalled card). A network hiccup
becomes a recoverable gap instead of a dropout.

**The daemon knows nothing about Samo's catalog.** samo-server resolves every
item to an absolute stream URL before sending it. The one exception is the
fallback station, whose URL the daemon builds itself — it has to be able to tune
that with nobody around to ask. That is the only reason it knows a station has
two kinds: a channel is `/channels/{id}/stream`, an internet station is
`/internet-radio/{id}/stream`, and everything after that is identical.

**The Samo token only goes to Samo.** A channel can contain a third-party
internet station, so the Authorization header is attached by URL prefix, never
blanket-applied.

**Samo decides levels; the daemon only applies them.** An item can carry a
`gainDb` — a constant offset in decibels that makes a podcast and a pop master
come out of the aux port at the same perceived volume. It is applied in the
decode chain as `volume=NdB`, which is one multiplication over every sample, so
the item's own dynamics survive untouched. Nothing here compresses anything: the
only other filter that can appear is a bounded true-peak limiter, and only for
the rare quiet-but-peaky item that would otherwise overshoot when lifted.

The measurement behind that number is EBU R128 analysis of the file, which needs
the catalog, somewhere to cache the answer, and CPU to spare — the same argument
that keeps URL resolution on the server. Programme level and listener volume
also stay separate on purpose: `gainDb` lives in the decoder, the volume knob
lives in the sink.

## Install

On the server, as root:

```bash
sudo ./packaging/install.sh
```

That installs the binary to `/usr/local/bin`, creates a `samo-radio` system user
in the `audio` group, writes `/var/lib/samo-radio/config.json` with a generated
control token, and enables the systemd unit. It prints the token and the list of
audio outputs when it finishes.

Then, in Samo: **RADIO → SAMO-RADIO → + ADD DEVICE**, paste the control token,
pick the output device and a default channel. Everything after install is done
from the UI.

It also unmutes the sound card if nothing else has — see Troubleshooting.

Requirements: `ffmpeg` and `alsa-utils` (aplay, amixer, alsactl), or
`pulseaudio-utils` (paplay) for the Pulse path. The installer apt-gets whatever
is missing.

## Picking the output

```bash
samo-radio --devices
```

Starred entries are the ones to try first. On ALSA, prefer `plughw:…` over
`hw:…`: the plug layer converts sample rate and format in software, so a card
that only does 44.1kHz still accepts the 48kHz stream instead of failing to
open. `default` is usually right on a machine with one card.

The analog line-out is generally the entry whose description mentions *Analog*.
If the box has HDMI audio it will also appear here; pick the one that isn't it.

## Control API

Loopback only by default (`127.0.0.1:7970`), with a bearer control token.

| | | |
|---|---|---|
| `GET` | `/v1/health` | liveness, unauthenticated |
| `GET` | `/v1/state` | full snapshot |
| `GET` | `/v1/events` | SSE, one snapshot per second |
| `GET` | `/v1/outputs` | audio devices, with the current selection |
| `GET` | `/v1/channels` | proxied Samo channel list |
| `POST` | `/v1/pair` | `{serverUrl, token, serverName, deviceName}` |
| `POST` | `/v1/play` | `{mode:"channel", channelId}`, `{mode:"station", stationId}`, or `{mode:"queue", items[], startIndex}` |
| `POST` | `/v1/enqueue` | append `items[]` to a running queue |
| `POST` | `/v1/pause` `/v1/resume` `/v1/next` `/v1/previous` | transport |
| `POST` | `/v1/stop` | end the ad-hoc queue, return to the station |
| `POST` | `/v1/standby` | genuinely silent; sink stays open |
| `POST` | `/v1/seek` | `{positionSeconds}` |
| `POST` | `/v1/volume` | `{volume}` 0–1 |
| `PATCH` | `/v1/settings` | device name, `defaultStation {kind,id,name}`, auto-tune, output |

Every command returns the new state, so a client never has to follow one with a
fetch.

## Configuration

`/var/lib/samo-radio/config.json`, written by the daemon. Environment variables
override the file, for a systemd drop-in that needs to pin something:
`SAMO_RADIO_CONFIG`, `SAMO_RADIO_ADDR`, `SAMO_RADIO_CONTROL_TOKEN`,
`SAMO_RADIO_SERVER_URL`, `SAMO_RADIO_SERVER_TOKEN`, `SAMO_RADIO_BACKEND`,
`SAMO_RADIO_DEVICE`, `SAMO_RADIO_SAMPLE_RATE`, `SAMO_RADIO_FFMPEG`,
`SAMO_RADIO_NAME`.

## Troubleshooting

```bash
journalctl -u samo-radio -f
curl -s localhost:7970/v1/health | jq
```

- **"Permission denied" opening the device** — the service user is not in
  `audio`. `sudo usermod -aG audio samo-radio && sudo systemctl restart samo-radio`.
- **"Device or resource busy"** — something else owns the card (a desktop
  session running PipeWire). Set the backend to `pulse` in the Samo UI.
- **Silence, no errors** — the card's own mixer is muted or at zero, which is
  invisible to samo-radio: it will happily report "playing" with the position
  advancing. The installer handles this, and you can re-run it any time:

  ```bash
  sudo samo-radio --unmute
  ```

  It is deliberately narrow about what it will touch, because a mute means
  different things in different places:

  - **`Master` and `PCM`** gate every output on the card. Silenced there,
    nothing comes out of anything, and no setup wants that — so those get
    fixed.
  - **`Speaker`, `Headphone`, `Line Out`, `Front`** choose which socket is
    live. A muted Headphone with nothing plugged in is *correct*. These are
    only touched when **every one of them is silenced**, meaning nothing is
    routed anywhere and there is no decision to preserve. If even one output
    is live, the mutes are how the machine is wired and it reports them
    instead of overriding them.

  A level you set on purpose always survives — only muted or zeroed controls
  are candidates at all. Levels go to `0dB` (unity) rather than 100%: unity is
  what the DAC is built to output, and on codecs that offer gain above it,
  100% would clip. The mixer state is saved afterwards so a reboot does not
  come back silent.
- **Stuttering** — raise `bufferMillis` from 300.
