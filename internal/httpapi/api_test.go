package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bouliehaan/samo-radio/internal/config"
	"github.com/bouliehaan/samo-radio/internal/player"
)

func newTestHandler(t *testing.T, token string) *Handler {
	t.Helper()
	store, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := store.Update(func(c *config.Config) error {
		c.ControlToken = token
		return nil
	}); err != nil {
		t.Fatalf("set token: %v", err)
	}
	return New(player.New(store, nil), store, nil)
}

func get(t *testing.T, handler *Handler, path, token string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

// The daemon listens on the LAN, so the control token is the only thing between
// a stranger on the network and the speakers.
func TestControlRoutesRequireTheToken(t *testing.T) {
	handler := newTestHandler(t, "s3cret")

	if code := get(t, handler, "/v1/state", "s3cret"); code != http.StatusOK {
		t.Fatalf("the right token should be let through, got %d", code)
	}
	if code := get(t, handler, "/v1/state", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token should be refused, got %d", code)
	}
	if code := get(t, handler, "/v1/state", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token at all should be refused, got %d", code)
	}
}

// A device whose token has been blanked by hand serves nothing. This used to
// wave every request through, which was survivable on loopback and is not on a
// device that ships listening to the network.
func TestAnEmptyControlTokenFailsClosed(t *testing.T) {
	handler := newTestHandler(t, "")
	if code := get(t, handler, "/v1/state", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("expected the API to refuse to serve without a token, got %d", code)
	}
}

// Health stays open: systemd, a watchdog or a curl from the console must be
// able to ask "is it alive" without credentials, and it leaks nothing else.
func TestHealthIsUnauthenticated(t *testing.T) {
	handler := newTestHandler(t, "s3cret")
	if code := get(t, handler, "/v1/health", ""); code != http.StatusOK {
		t.Fatalf("health should answer without a token, got %d", code)
	}
}

// The peer address decides where a remote device fetches its audio from, so it
// has to survive the forms Go actually hands over.
func TestCallerHostNormalisesPeerAddresses(t *testing.T) {
	for remote, want := range map[string]string{
		"192.168.1.10:54321":        "192.168.1.10",
		"[::ffff:192.168.1.10]:443": "192.168.1.10",
		"[2001:db8::10]:54321":      "2001:db8::10",
		"[fe80::1%eth0]:54321":      "fe80::1",
		"192.168.1.10":              "192.168.1.10",
		"":                          "",
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/pair", nil)
		request.RemoteAddr = remote
		if got := callerHost(request); got != want {
			t.Fatalf("callerHost(%q) = %q, want %q", remote, got, want)
		}
	}
}
