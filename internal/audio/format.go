// Package audio holds the PCM primitives the rest of samo-radio is built on:
// the wire format everything is decoded into, and the jitter buffer that sits
// between a network source and the sound card.
package audio

import "fmt"

// Format is the PCM shape the whole pipeline speaks. Every source is decoded
// into this by ffmpeg and the sink is opened with exactly these parameters, so
// nothing downstream ever has to resample or reopen the device mid-stream.
//
// Signed 16-bit little-endian is not a quality ceiling anyone will hear on a
// 3.5mm line-out, and it buys the thing that actually matters here: a single
// sound-card handle that stays open from boot to shutdown. A format that
// changed per track would mean closing and reopening the device between songs,
// which is where the pops, the races and the "device or resource busy" errors
// live.
type Format struct {
	SampleRate int
	Channels   int
}

// DefaultFormat is CD-plus rate stereo. 48kHz rather than 44.1 because it is
// what essentially every modern codec, card and HDMI path prefers natively.
var DefaultFormat = Format{SampleRate: 48000, Channels: 2}

// BytesPerSample is fixed at 2 (S16_LE). It is a constant rather than a field
// because the sink command lines, the gain code and the ring sizing all encode
// the same assumption; making it configurable without changing those would be
// a lie.
const BytesPerSample = 2

func (f Format) Validate() error {
	if f.SampleRate < 8000 || f.SampleRate > 384000 {
		return fmt.Errorf("sample rate %d out of range", f.SampleRate)
	}
	if f.Channels < 1 || f.Channels > 8 {
		return fmt.Errorf("channel count %d out of range", f.Channels)
	}
	return nil
}

// BytesPerFrame is one sample across every channel.
func (f Format) BytesPerFrame() int { return f.Channels * BytesPerSample }

// BytesPerSecond is the sink's clock rate: how fast the sound card drains what
// we write to it, and therefore how byte counts convert to wall time.
func (f Format) BytesPerSecond() int { return f.SampleRate * f.BytesPerFrame() }

// BytesForDuration rounds down to a whole frame so a buffer never holds a
// fraction of a sample, which would desync the channel interleave.
func (f Format) BytesForDuration(seconds float64) int {
	if seconds <= 0 {
		return 0
	}
	raw := int(float64(f.BytesPerSecond()) * seconds)
	return raw - raw%f.BytesPerFrame()
}

// SecondsForBytes converts a written/consumed byte count back to elapsed time.
func (f Format) SecondsForBytes(n int64) float64 {
	if n <= 0 {
		return 0
	}
	return float64(n) / float64(f.BytesPerSecond())
}
