# samo-radio

A headless Samo playback device. It holds a sound card open and plays whatever
[Samo](../samo-server) tells it to — out the box's own line-out, into whatever
is plugged in.

Two things in one:

- **A station.** It tunes a Samo channel *or* an internet radio station and
  stays there. On boot, after a power cut, after a network drop, it comes back
  on air by itself. Nobody has to log in.
- **A cast target.** From the phone or the web UI, "play to samo-radio" sends a
  queue to it — the aux port picks it up, and when the queue runs out it drops
  back to the station.

## Where it runs

Any Linux box on the same network as Samo, with a sound card. There are two
shapes and the daemon does not distinguish between them:

- **On the samo-server machine**, playing out of its line-out into an amp. One
  box, no extra hardware, and the two halves talk over loopback.
- **On its own box.** A Pi, a power supply, speakers in the plug socket: that is
  a Samo endpoint. Install, pair, pick a station, walk away. Put one in the
  kitchen and one in the workshop and they are two independent devices in the
  RADIO panel, each on its own station.

Same binary, same install script, same config file. The only thing that differs
is which addresses the two ends use to reach each other, and both are worked out
during pairing rather than configured by hand.

## How it fits together

```
  phone / web UI                samo-server                 samo-radio
        │                            │                          │
        │  POST /api/v1/samo-radio/  │  POST /v1/play           │
        │  devices/{id}/play         │  (control token)         │
        ├───────────────────────────►├─────────────────────────►│
        │                            │  (resolved stream URLs)  │
        │                            │                          ▼
        │                            │◄───── GET /channels/…/stream
        │                            │       GET /api/v1/…/stream
        │                            │           (device token) │
        │                            │                          ▼
        │                            │                    ffmpeg → PCM
        │                            │                          ▼
        │                            │                  ┌───────────────┐
        │                            │                  │ always-open   │
        │                            │                  │ sink (aplay)  │──► 🔊 aux
        │                            │                  └───────────────┘
                                     └── loopback, or the LAN ──┘
```

Clients never talk to this daemon directly. samo-server proxies every command,
so control works from anywhere Samo works — including through a tunnel — and
this process never has to grow its own accounts, sessions or TLS.

Both of the arrows crossing to the right-hand column may be loopback or may be
the network; nothing in the daemon cares which. Note that they point in opposite
directions and carry different credentials: Samo proves itself to the device
with the **control token**, and the device proves itself to Samo with its own
**device token**, minted during pairing.

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

**The control token is the whole of the door.** The API answers on every
interface, because a device that only answered on its own loopback would be one
nobody could add unless Samo happened to be on the same box. What makes that
safe is the token: every route but `/v1/health` requires it, and a device with
no token configured refuses to serve rather than serving whoever asks. The
daemon mints one on first start, so there is no window in which a box is up and
unprotected.

It is a shared secret over plain HTTP, which is the right size for what it
protects — the speakers in your kitchen, on your own network. It is not a
credential to expose to the internet: keep the device on the LAN and let Samo,
which has accounts and TLS and a tunnel, be the thing that faces outward.

**The device works out where Samo is, if Samo gets it wrong.** Samo tells the
device what URL to fetch audio from, and defaults to its own loopback address —
correct when the two are on one machine, useless on a Pi across the house. The
device is the end that can tell the difference: a pairing request that arrived
from `192.168.1.10` came from a Samo at `192.168.1.10`, whatever the body says.
So the supplied URL is tried first, and if it does not answer, the address the
request actually came from is tried instead. That address is the TCP peer of an
already-authenticated request, so trusting it is no weaker than trusting the
body it arrived in.

Pairing proves the credentials work before storing them, and the whole
verification is bounded well under the six seconds Samo allows for the call —
otherwise Samo gives up first and revokes a token the device has already saved.

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

On the box with the speakers — the samo-server machine, a Pi, anything Linux —
as root:

```bash
sudo ./packaging/install.sh
```

That installs the binary to `/usr/local/bin`, creates a `samo-radio` system user
in the `audio` group, creates `/var/lib/samo-radio/config.json`, and enables the
systemd unit. When it finishes it prints the audio outputs it can see and the
three things Samo needs: the device's name, the URLs it answers on, and its
control token.

Requirements: `ffmpeg` and `alsa-utils` (aplay, amixer, alsactl), or
`pulseaudio-utils` (paplay) for the Pulse path. The installer apt-gets whatever
is missing. It also unmutes the sound card if nothing else has — see
Troubleshooting.

If the box has no Go toolchain, build elsewhere and ship the binary with the
`packaging/` directory beside it; the installer uses a prebuilt `./samo-radio`
when it finds one.

```bash
GOARCH=arm64 make build-linux    # or `make build-pi`, which is the same thing
```

The device listens on `0.0.0.0:7970` so Samo can reach it from wherever it runs.
On a machine where Samo is a local process and nothing else should be able to
knock, pin it to loopback instead:

```bash
sudo LISTEN_ADDR=127.0.0.1:7970 ./packaging/install.sh
```

## Pairing

In Samo: **RADIO → SAMO-RADIO → + ADD DEVICE**. It asks for a name, the device's
control URL and the control token. To print all three again later:

```bash
sudo samo-radio --pairing
```

An unpaired device also logs its addresses at every boot, so `journalctl -u
samo-radio` answers "which one did it get" on a box you cannot see. The token
goes into the journal exactly once, on the boot that mints it; after that
`--pairing` is the way to read it back.

Pairing is immediate: Samo mints a token for the device, the device checks the
token works before storing it, and it comes back with an error rather than a
half-finished device if it does not. Then pick the output and a default station
in the device's settings. Everything after install is done from the UI.

A headless box is easier to reach by name than by address — `avahi-daemon` is
already running on Raspberry Pi OS, which makes `http://samo-pi.local:7970`
work and survive the DHCP lease changing. On a minimal Debian, `sudo apt-get
install avahi-daemon` buys the same thing.

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

The output can be changed from the Samo UI at any time and takes effect without
a restart, which matters because finding the right socket is trial and error and
doing it over SSH is the experience this project exists to avoid.

### On a Raspberry Pi

The card shows up as `Headphones` (`plughw:CARD=Headphones,DEV=0`), and on a Pi
4 or earlier that is the 3.5mm jack. It works, and it is a PWM output on a noisy
board: fine for a workshop, audibly hissy through anything revealing. A £10 USB
DAC or an I²S HAT is a large improvement for the money, and appears in
`--devices` like any other card. The Pi 5 has no analog jack at all, so there
one of those is not optional.

If no card appears, onboard audio is switched off: set `dtparam=audio=on` in
`/boot/firmware/config.txt` and reboot. If a USB DAC enumerates late in boot,
the daemon may start before the card exists, fail to open it, and be restarted
by systemd two seconds later — which is the intended outcome, not a fault to
chase.

## Control API

`0.0.0.0:7970` by default, with a bearer control token on every route but
health. It is meant for samo-server, not for clients.

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

`listenAddr` accepts `7970`, `:7970` or `host:port`, and a bare port means every
interface. Changing it needs a restart — it is the one setting the API cannot
change on itself, since moving the socket would cut the connection carrying the
request. An install that predates network-reachable devices keeps whatever is in
its config file, so a device that has always been on `127.0.0.1:7970` stays
there across an upgrade until somebody says otherwise.

## Troubleshooting

```bash
journalctl -u samo-radio -f
curl -s localhost:7970/v1/health | jq
```

- **Samo cannot reach the device** — check the address first: `sudo samo-radio
  --pairing` prints the ones it answers on. If the device is listening on
  `127.0.0.1:7970` it is reachable from its own machine and nowhere else. A
  `401` is the other half of the same problem — the device has a control token
  and Samo has a different one; re-pair, or paste the printed token again.
- **Paired, but it never plays and the log says "connection refused"** — the
  device is being sent to fetch audio from an address that means nothing on its
  own machine, usually `127.0.0.1:6969` from a Samo somewhere else. Re-pair: the
  device retries against whatever address the pairing request came from. If Samo
  reaches it through a proxy or a tunnel, set the device's stream base URL
  explicitly on the Samo side, since the address the request arrives from is
  then the proxy's rather than Samo's.
- **"Permission denied" opening the device** — the service user is not in
  `audio`. `sudo usermod -aG audio samo-radio && sudo systemctl restart samo-radio`.
- **"Device or resource busy"** — something else owns the card (a desktop
  session running PipeWire, which a Pi with a desktop image has). Set the
  backend to `pulse` in the Samo UI.
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
