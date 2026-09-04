# samo-radio — design notes

Why the daemon is shaped the way it is. For installing and running it, see the
[README](../README.md).

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
