package player

import (
	"strings"
	"testing"
)

// The whole point of the fallback: Samo hands out its own loopback address by
// default, which is a dead end on a device that is not the server. The address
// the request came from is the one that can actually be reached.
func TestPairingFallsBackToTheAddressSamoCalledFrom(t *testing.T) {
	candidates := serverURLCandidates("http://127.0.0.1:6969", "192.168.1.10")
	if len(candidates) != 2 {
		t.Fatalf("expected the given URL and a fallback, got %v", candidates)
	}
	// The given URL still goes first: on a box running both, loopback is right
	// and must remain the address that gets stored.
	if candidates[0] != "http://127.0.0.1:6969" {
		t.Fatalf("the URL Samo supplied must be tried first, got %q", candidates[0])
	}
	if candidates[1] != "http://192.168.1.10:6969" {
		t.Fatalf("fallback should keep the port and swap the host, got %q", candidates[1])
	}
}

// A caller that is genuinely on this machine tells us nothing we did not
// already have, and a URL that already names a real host is not ours to
// second-guess — Samo behind a proxy or a tunnel is Samo's business.
func TestPairingLeavesUsableURLsAlone(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		serverURL  string
		callerHost string
	}{
		{"same machine", "http://127.0.0.1:6969", "127.0.0.1"},
		{"localhost from the same machine", "http://localhost:6969", "::1"},
		{"a routable URL", "https://samo.example.com", "192.168.1.10"},
		{"a routable URL over the LAN", "http://192.168.1.10:6969", "192.168.1.10"},
		{"no caller address", "http://127.0.0.1:6969", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidates := serverURLCandidates(testCase.serverURL, testCase.callerHost)
			if len(candidates) != 1 {
				t.Fatalf("expected the supplied URL only, got %v", candidates)
			}
		})
	}
}

func TestPairingRewriteKeepsSchemeAndPath(t *testing.T) {
	candidates := serverURLCandidates("https://localhost:6969/samo/", "10.0.0.5")
	if len(candidates) != 2 {
		t.Fatalf("expected a fallback, got %v", candidates)
	}
	if candidates[1] != "https://10.0.0.5:6969/samo" {
		t.Fatalf("scheme or path was lost: %q", candidates[1])
	}
}

// An IPv6 caller has to come back as a URL host, brackets and all, or the
// fallback produces an address nothing can parse.
func TestPairingRewriteBracketsIPv6Callers(t *testing.T) {
	candidates := serverURLCandidates("http://127.0.0.1:6969", "2001:db8::10")
	if len(candidates) != 2 {
		t.Fatalf("expected a fallback, got %v", candidates)
	}
	if candidates[1] != "http://[2001:db8::10]:6969" {
		t.Fatalf("IPv6 caller was not bracketed: %q", candidates[1])
	}
}

// The failure a remote device hits first deserves better than "connection
// refused": the address it was handed is not wrong-looking, it is just not an
// address that means anything here.
func TestPairFailureExplainsALoopbackServerURL(t *testing.T) {
	err := pairFailure([]string{"http://127.0.0.1:6969"}, errAssert)
	if !strings.Contains(err.Error(), "another machine") {
		t.Fatalf("unhelpful pairing error: %v", err)
	}

	both := pairFailure([]string{"http://127.0.0.1:6969", "http://192.168.1.10:6969"}, errAssert)
	if !strings.Contains(both.Error(), "192.168.1.10") {
		t.Fatalf("the error should name every address tried: %v", both)
	}
}

var errAssert = errTest("connection refused")

type errTest string

func (e errTest) Error() string { return string(e) }
