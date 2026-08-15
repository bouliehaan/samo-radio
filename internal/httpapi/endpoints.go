package httpapi

import (
	"net"
	"sort"
	"strings"
)

// Endpoints lists the URLs this device's control API can be reached on.
//
// It exists because of one setup question: somebody has just plugged a Pi into
// an amp and now has to type the device's address into Samo on another
// machine. Guessing it means a router page or an nmap sweep; printing it means
// reading it off the console or out of the journal. Enumeration happens at
// call time rather than at boot — a box on wifi and DHCP does not have the same
// address next month, and a stale answer is worse than none.
func Endpoints(addr string) []string {
	return endpointsFor(addr, interfaceIPs())
}

// endpointsFor is the pure half, so the formatting is testable without
// depending on whatever interfaces the test machine happens to have.
func endpointsFor(addr string, candidates []net.IP) []string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		// Not host:port. Nothing sensible to expand, so hand back what was
		// configured rather than inventing an address.
		return []string{"http://" + strings.TrimSpace(addr)}
	}
	host = strings.Trim(host, "[]")

	// A specific bind is its own answer: it is the only address that answers.
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}

	urls := make([]string, 0, len(candidates))
	for _, ip := range candidates {
		urls = append(urls, "http://"+net.JoinHostPort(ip.String(), port))
	}
	if len(urls) == 0 {
		// Wildcard bind on a box with no network yet. Loopback is the one
		// address that is always there.
		return []string{"http://" + net.JoinHostPort("127.0.0.1", port)}
	}
	sort.Strings(urls)
	return urls
}

// interfaceIPs returns the addresses another machine could plausibly use.
//
// IPv4 only, and no loopback: the point is what to paste into Samo running
// somewhere else. Link-local addresses (169.254/16, fe80::) are dropped for the
// same reason — an interface that failed to get a lease is not a way in.
func interfaceIPs() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var found []net.IP
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				found = append(found, v4)
			}
		}
	}
	return found
}
