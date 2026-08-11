package source

import (
	"strings"
	"testing"

	"github.com/bouliehaan/samo-radio/internal/audio"
)

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsSeeksBeforeInput(t *testing.T) {
	args := buildArgs(Options{URL: "/srv/media/song.flac", StartSeconds: 90}, audio.DefaultFormat)
	seek := indexOf(args, "-ss")
	input := indexOf(args, "-i")
	if seek < 0 || input < 0 {
		t.Fatalf("missing -ss or -i: %v", args)
	}
	// -ss before -i is the fast path: ffmpeg jumps in the container instead of
	// decoding and discarding everything up to the mark.
	if seek > input {
		t.Fatalf("-ss must come before -i, got %v", args)
	}
	if args[seek+1] != "90.000" {
		t.Fatalf("unexpected seek value %q", args[seek+1])
	}
}

// Reconnect options belong to the http protocol; passing them for a local file
// makes ffmpeg exit with "Option not found" and the track never plays.
func TestBuildArgsOmitsHTTPOptionsForLocalFiles(t *testing.T) {
	args := buildArgs(Options{URL: "/srv/media/song.flac"}, audio.DefaultFormat)
	if strings.Contains(joined(args), "-reconnect") {
		t.Fatalf("local file input must not carry http reconnect options: %v", args)
	}
	if strings.Contains(joined(args), "-headers") {
		t.Fatalf("local file input must not carry headers: %v", args)
	}
}

func TestBuildArgsHTTPCarriesHeadersAndReconnect(t *testing.T) {
	args := buildArgs(Options{
		URL:     "https://samo.example.com/channels/x/stream",
		Headers: map[string]string{"Authorization": "Bearer abc"},
		Live:    true,
	}, audio.DefaultFormat)
	text := joined(args)
	if !strings.Contains(text, "Authorization: Bearer abc") {
		t.Fatalf("expected the auth header, got %v", args)
	}
	if !strings.Contains(text, "-reconnect_at_eof 1") {
		t.Fatalf("a live source should ride out an EOF, got %v", args)
	}
	// A live stream has no zero point to seek from.
	if strings.Contains(text, "-ss") {
		t.Fatalf("live sources must not be seeked: %v", args)
	}
}

func TestBuildArgsProducesRawPCMAtSinkFormat(t *testing.T) {
	format := audio.Format{SampleRate: 44100, Channels: 2}
	args := buildArgs(Options{URL: "https://example.com/a.mp3"}, format)
	text := joined(args)
	for _, want := range []string{"-f s16le", "-ar 44100", "-ac 2", "-vn"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in %v", want, args)
		}
	}
	if args[len(args)-1] != "-" {
		t.Fatalf("output must go to stdout, got %q", args[len(args)-1])
	}
}

func TestBuildArgsHeaderTerminatesWithCRLF(t *testing.T) {
	args := buildArgs(Options{
		URL:     "http://samo.local/x",
		Headers: map[string]string{"Authorization": "Bearer abc"},
	}, audio.DefaultFormat)
	value := args[indexOf(args, "-headers")+1]
	if !strings.HasSuffix(value, "\r\n") {
		t.Fatalf("ffmpeg expects CRLF-terminated headers, got %q", value)
	}
}

func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

// ----- loudness levelling ----------------------------------------------

// Samo works out the gain and ships it with the item; this end only applies
// it. The filter has to sit after -i, or ffmpeg has nothing decoded to attach
// it to.
func TestBuildArgsAppliesItemGain(t *testing.T) {
	args := buildArgs(Options{URL: "/srv/media/quiet.flac", GainDB: 7.5}, audio.DefaultFormat)
	af := indexOf(args, "-af")
	if af < 0 {
		t.Fatalf("missing -af: %v", args)
	}
	if af < indexOf(args, "-i") {
		t.Fatalf("-af must come after -i, got %v", args)
	}
	if args[af+1] != "volume=7.5dB" {
		t.Fatalf("filter = %q, want a plain constant gain", args[af+1])
	}
	// A constant gain and nothing else. Any dynamics processor here would
	// flatten the item's own light and shade, which is the whole thing this
	// design avoids.
	for _, banned := range []string{"dynaudnorm", "loudnorm", "compand", "acompressor"} {
		if strings.Contains(joined(args), banned) {
			t.Fatalf("%s must never appear in a playback chain: %v", banned, args)
		}
	}
}

// An item with no gain must be piped through untouched — no filter, no float
// round trip, no behaviour change from before levelling existed.
func TestBuildArgsOmitsFilterWithoutGain(t *testing.T) {
	args := buildArgs(Options{URL: "/srv/media/song.flac"}, audio.DefaultFormat)
	if indexOf(args, "-af") >= 0 {
		t.Fatalf("unlevelled item must get no -af at all: %v", args)
	}
}

func TestBuildArgsLimiterUsesCeilingAndNeverRelevels(t *testing.T) {
	args := buildArgs(Options{
		URL: "/srv/media/dynamic.flac", GainDB: 5.5, LimitPeaks: true, CeilingDBTP: -1,
	}, audio.DefaultFormat)
	af := indexOf(args, "-af")
	if af < 0 {
		t.Fatalf("missing -af: %v", args)
	}
	want := "volume=5.5dB,alimiter=limit=0.8913:attack=5:release=60:level=disabled"
	if args[af+1] != want {
		t.Fatalf("filter = %q\nwant     = %q", args[af+1], want)
	}
}

// A limiter with no explicit ceiling must still leave headroom. The sink is
// 16-bit and clips hard, so defaulting the ceiling to 0 dBFS would turn the
// safety net into the fault it exists to prevent.
func TestBuildArgsLimiterDefaultsToOneDBOfHeadroom(t *testing.T) {
	args := buildArgs(Options{URL: "/x.flac", GainDB: 3, LimitPeaks: true}, audio.DefaultFormat)
	if filter := args[indexOf(args, "-af")+1]; !strings.Contains(filter, "limit=0.8913") {
		t.Fatalf("filter = %q, want the -1 dBTP default ceiling", filter)
	}
}
