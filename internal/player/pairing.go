package player

import (
	"net"
	"net/url"
	"strings"
)

// PairRequest is everything a pairing call carries.
type PairRequest struct {
	// ServerURL is where the device should reach Samo to pull audio.
	ServerURL string
	// Token is the Samo API token minted for this device.
	Token string
	// ServerName and DeviceName are cosmetic, for the device's status page.
	ServerName string
	DeviceName string
	// CallerHost is the address the pairing request actually arrived from.
	//
	// It is here because of one asymmetry in how a device is registered. Samo
	// is told where the device is — somebody types it in — but the device is
	// told where Samo is by Samo, which defaults to its own loopback address
	// on the reasonable assumption that the device is next to it. On a Pi
	// across the house that address is a dead end, and the failure is baffling:
	// pairing succeeds, then nothing ever plays, because 127.0.0.1:6969 on the
	// Pi is nothing at all.
	//
	// The device is the one end that knows better. A pairing request that
	// arrived from 192.168.1.10 came from a Samo that is at 192.168.1.10,
	// whatever the body claims, so that is the fallback when the URL it was
	// given does not answer. The address is the TCP peer of an already
	// authenticated request, so trusting it is no weaker than trusting the body
	// it came in.
	CallerHost string
}

// serverURLCandidates lists the addresses to try for Samo, best first.
//
// The URL Samo supplied is always tried first: when the two really are on the
// same box, loopback is the right answer and must stay the stored one. Only a
// loopback URL from a non-loopback caller earns a second candidate, because
// that combination cannot be right — a caller reaching us over the network
// cannot also be on our loopback interface.
func serverURLCandidates(rawURL, callerHost string) []string {
	given := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if given == "" {
		return nil
	}
	rewritten, ok := swapHost(given, callerHost)
	if !ok || rewritten == given {
		return []string{given}
	}
	return []string{given, rewritten}
}

// swapHost rewrites a loopback URL to point at the caller instead, keeping the
// scheme, port and path it was given.
func swapHost(rawURL, callerHost string) (string, bool) {
	callerHost = strings.TrimSpace(callerHost)
	if callerHost == "" || isLoopbackHost(callerHost) {
		return "", false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || !isLoopbackHost(parsed.Hostname()) {
		return "", false
	}
	host := callerHost
	if ip := net.ParseIP(callerHost); ip != nil && ip.To4() == nil {
		host = "[" + callerHost + "]"
	}
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(callerHost, port)
	}
	parsed.Host = host
	return strings.TrimRight(parsed.String(), "/"), true
}

// isLoopbackHost reports whether a URL host or peer address means "this
// machine, and only this machine".
func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	host, _, _ = strings.Cut(host, "%")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
