package httpapi

import (
	"net"
	"reflect"
	"testing"
)

// A wildcard bind is the default, and the reason this exists: the operator has
// to type one of these into Samo on another machine.
func TestEndpointsExpandsAWildcardBind(t *testing.T) {
	got := endpointsFor("0.0.0.0:7970", []net.IP{net.ParseIP("192.168.1.42"), net.ParseIP("10.0.0.7")})
	want := []string{"http://10.0.0.7:7970", "http://192.168.1.42:7970"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A device pinned to one address answers on exactly that address, so listing
// the machine's other interfaces would be a lie.
func TestEndpointsRespectsASpecificBind(t *testing.T) {
	got := endpointsFor("127.0.0.1:7970", []net.IP{net.ParseIP("192.168.1.42")})
	if !reflect.DeepEqual(got, []string{"http://127.0.0.1:7970"}) {
		t.Fatalf("got %v", got)
	}
}

// A box whose network has not come up yet still has to print something true.
func TestEndpointsFallsBackToLoopback(t *testing.T) {
	got := endpointsFor(":7970", nil)
	if !reflect.DeepEqual(got, []string{"http://127.0.0.1:7970"}) {
		t.Fatalf("got %v", got)
	}
}

func TestEndpointsPassesThroughSomethingUnparseable(t *testing.T) {
	got := endpointsFor("unix:/run/samo.sock", []net.IP{net.ParseIP("192.168.1.42")})
	if !reflect.DeepEqual(got, []string{"http://unix:/run/samo.sock"}) {
		t.Fatalf("got %v", got)
	}
}
