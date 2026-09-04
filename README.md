# samo-radio

A headless playback device for
[samo-server](https://github.com/bouliehaan/samo-server). It holds a Linux box's
sound card open and plays out the line-out, into whatever is plugged in.

- **A station.** It tunes a samo channel or an internet radio station and stays
  there. After a power cut, after a network drop, it comes back on air by
  itself. Nobody has to log in.
- **A cast target.** From the phone or the web UI, "play to samo-radio" sends a
  queue to it, and when the queue runs out it drops back to the station.

Runs on the samo-server box itself, or on its own Pi in another room. Same
binary either way — the addresses are worked out during pairing, not configured.

## Install

Not a container: it needs the machine's real sound card, so it runs as a
systemd service on the box with the speakers. Five commands, as root.

Its dependencies first — `ffmpeg`, plus `alsa-utils` for the ALSA path or
`pulseaudio-utils` if the box already runs PulseAudio/PipeWire:

```bash
sudo apt-get install -y ffmpeg alsa-utils
```

The binary (`…-linux-amd64` on an x86 box):

```bash
sudo curl -fsSL -o /usr/local/bin/samo-radio \
  https://github.com/bouliehaan/samo-radio/releases/latest/download/samo-radio-linux-arm64
sudo chmod +x /usr/local/bin/samo-radio
```

An unprivileged account for it, in `audio` so it can open `/dev/snd`:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin samo-radio
sudo usermod -aG audio samo-radio
```

The unit — it creates `/var/lib/samo-radio` itself, and the daemon writes its
own config there on first start and mints its own control token:

```bash
sudo curl -fsSL -o /etc/systemd/system/samo-radio.service \
  https://raw.githubusercontent.com/bouliehaan/samo-radio/main/packaging/samo-radio.service
sudo systemctl daemon-reload
sudo systemctl enable --now samo-radio
```

Then read back the three things samo needs — the device's name, the URLs it
answers on, and its control token:

```bash
sudo -u samo-radio samo-radio --pairing
```

To upgrade, replace the binary and `sudo systemctl restart samo-radio`. Write
the new one beside the old and rename it (`curl -o /usr/local/bin/.samo-radio.new`
then `mv -f`) — Linux refuses to open a running executable for writing, so
downloading straight over the top fails with "Text file busy".

The unit leaves the daemon listening on `0.0.0.0:7970`, so samo can reach it
from wherever it runs. On a box where samo is a local process and nothing else
should be able to knock, put `"listenAddr": "127.0.0.1:7970"` in
`/var/lib/samo-radio/config.json` and restart.

Building it yourself instead of taking the release binary:

```bash
CGO_ENABLED=0 go build -trimpath -o samo-radio ./cmd/samo-radio
```

## Pairing

In samo: **RADIO → SAMO-RADIO → + ADD DEVICE**. It wants a name, the device's
control URL and the control token. To print all three again:

```bash
sudo samo-radio --pairing
```

An unpaired device also logs its addresses at every boot, so `journalctl -u
samo-radio` answers "which address did it get" on a box you cannot see. The
token reaches the journal exactly once, on the boot that mints it.

Pairing is immediate, and everything after it — output, default station, name —
is done from the samo UI. On a headless box, `avahi-daemon` (already running on
Raspberry Pi OS) makes `http://samo-pi.local:7970` work and survive a DHCP lease
change.

## Picking the output

```bash
samo-radio --devices
```

Starred entries are the ones to try first. On ALSA prefer `plughw:…` over
`hw:…` — the plug layer converts rate and format in software, so a card that
only does 44.1kHz still accepts the 48kHz stream instead of failing to open.
`default` is usually right on a machine with one card. The analog line-out is
generally the entry mentioning *Analog*; if the box has HDMI audio, pick the one
that isn't it.

The output can be changed from the samo UI at any time, without a restart.

**On a Raspberry Pi**, the card is `Headphones`
(`plughw:CARD=Headphones,DEV=0`) and on a Pi 4 or earlier that is the 3.5mm
jack. It works, and it is a PWM output on a noisy board — fine for a workshop,
audibly hissy through anything revealing. A £10 USB DAC or an I²S HAT is a large
improvement, and the Pi 5 has no analog jack at all, so there it is not
optional. If no card appears, onboard audio is off: set `dtparam=audio=on` in
`/boot/firmware/config.txt` and reboot.

## Control API

`0.0.0.0:7970`, bearer control token on every route but health. It is meant for
samo-server, not for clients — samo proxies every command, so control works from
anywhere samo works, including through a tunnel.

| | | |
|---|---|---|
| `GET` | `/v1/health` | liveness, unauthenticated |
| `GET` | `/v1/state` | full snapshot |
| `GET` | `/v1/events` | SSE, one snapshot per second |
| `GET` | `/v1/outputs` | audio devices, with the current selection |
| `GET` | `/v1/channels` | proxied samo channel list |
| `POST` | `/v1/pair` | `{serverUrl, token, serverName, deviceName}` |
| `POST` | `/v1/play` | `{mode:"channel", channelId}`, `{mode:"station", stationId}`, or `{mode:"queue", items[], startIndex}` |
| `POST` | `/v1/enqueue` | append `items[]` to a running queue |
| `POST` | `/v1/pause` `/v1/resume` `/v1/next` `/v1/previous` | transport |
| `POST` | `/v1/stop` | end the ad-hoc queue, return to the station |
| `POST` | `/v1/standby` | genuinely silent; sink stays open |
| `POST` | `/v1/seek` | `{positionSeconds}` |
| `POST` | `/v1/volume` | `{volume}` 0–1 |
| `PATCH` | `/v1/settings` | name, `defaultStation {kind,id,name}`, auto-tune, output |

Every command returns the new state, so a client never has to follow one with a
fetch.

## Configuration

`/var/lib/samo-radio/config.json`, written by the daemon. Environment variables
override it, for a systemd drop-in that needs to pin something:
`SAMO_RADIO_CONFIG`, `SAMO_RADIO_ADDR`, `SAMO_RADIO_CONTROL_TOKEN`,
`SAMO_RADIO_SERVER_URL`, `SAMO_RADIO_SERVER_TOKEN`, `SAMO_RADIO_BACKEND`,
`SAMO_RADIO_DEVICE`, `SAMO_RADIO_SAMPLE_RATE`, `SAMO_RADIO_FFMPEG`,
`SAMO_RADIO_NAME`.

`listenAddr` takes `7970`, `:7970` or `host:port`; a bare port means every
interface. It is the one setting the API cannot change on itself — moving the
socket would cut the connection carrying the request — so it needs a restart.

## Troubleshooting

```bash
journalctl -u samo-radio -f
curl -s localhost:7970/v1/health | jq
```

**samo cannot reach the device** — check the address: `sudo samo-radio
--pairing` prints the ones it answers on. A device on `127.0.0.1:7970` is
reachable from its own machine and nowhere else. A `401` is the same problem
from the other end: re-pair.

**Paired, but it never plays — "connection refused"** — it is being sent to
fetch audio from an address that means nothing on its own machine, usually
`127.0.0.1:6969` from a samo somewhere else. Re-pair; the device retries against
whatever address the pairing request came from. Behind a proxy or tunnel, set
the device's stream base URL explicitly on the samo side.

**"Permission denied" opening the device** — the service user is not in `audio`:
`sudo usermod -aG audio samo-radio && sudo systemctl restart samo-radio`.

**"Device or resource busy"** — something else owns the card, usually a desktop
session's PipeWire. Set the backend to `pulse` in the samo UI.

**Silence, no errors** — the card's own mixer is muted or at zero, which is
invisible to samo-radio: it reports "playing" with the position advancing.

```bash
sudo samo-radio --unmute
```

It is deliberately narrow. `Master` and `PCM` gate every output, so those get
fixed. `Speaker` / `Headphone` / `Line Out` / `Front` choose which socket is
live — a muted Headphone with nothing plugged in is *correct* — so those are
only touched when every one of them is silenced, meaning nothing is routed
anywhere and there is no decision to preserve. Levels go to `0dB` rather than
100%, because unity is what the DAC is built for and 100% clips on codecs that
offer gain above it.

**Stuttering** — raise `bufferMillis` from 300.

## Design notes

[docs/DESIGN.md](docs/DESIGN.md) — why the sound card is never closed, why the
sink is the clock, how the two tokens differ, and how the device works out where
samo is when samo gets it wrong.
