package clientip_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/simonjanss/rig/runtime/clientip"
)

func request(remote, forwarded string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	return r
}

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()

	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// The failure this package exists to prevent: a caller naming their own address
// and every limit keyed on one counting a different fiction each request.
func TestAnUntrustedPeerCannotNameItsOwnAddress(t *testing.T) {
	t.Parallel()

	for _, trusted := range [][]netip.Prefix{
		nil,
		prefixes(t, "10.0.0.0/8"),
	} {
		got := clientip.Of(request("198.51.100.7:44321", "203.0.113.1"), trusted)
		if got.String() != "198.51.100.7" {
			t.Errorf("with trusted=%v the header was believed: %s", trusted, got)
		}
	}
}

func TestATrustedProxyIsBelieved(t *testing.T) {
	t.Parallel()

	got := clientip.Of(request("10.1.2.3:44321", "203.0.113.1"), prefixes(t, "10.0.0.0/8"))
	if got.String() != "203.0.113.1" {
		t.Fatalf("got %s, want the forwarded client", got)
	}
}

// Everything after the left-most entry was added by a hop. Reading the last one
// would give the address of the proxy in front, which is one address for every
// caller — a limit that is either off or on for the whole internet.
func TestTheLeftMostForwardedEntryWins(t *testing.T) {
	t.Parallel()

	got := clientip.Of(
		request("10.1.2.3:44321", "203.0.113.1, 10.9.9.9, 10.1.2.3"),
		prefixes(t, "10.0.0.0/8"))
	if got.String() != "203.0.113.1" {
		t.Fatalf("got %s, want the original client", got)
	}
}

func TestAGarbledHeaderFallsBackToThePeer(t *testing.T) {
	t.Parallel()

	for _, forwarded := range []string{"not-an-address", ", 203.0.113.1", "  "} {
		got := clientip.Of(request("10.1.2.3:44321", forwarded), prefixes(t, "10.0.0.0/8"))
		if got.String() != "10.1.2.3" {
			t.Errorf("header %q produced %s rather than the peer", forwarded, got)
		}
	}
}

// One address, not two budgets. Both ways in: the peer, and the header a
// trusted proxy set — a mapped address there would key a caller separately from
// the same caller arriving through a proxy that spells it the other way.
func TestIPv4InIPv6IsUnmapped(t *testing.T) {
	t.Parallel()

	got := clientip.Parse("[::ffff:198.51.100.7]:44321")
	if got.String() != "198.51.100.7" {
		t.Fatalf("got %s", got)
	}

	forwarded := clientip.String(
		request("10.1.2.3:44321", "::ffff:198.51.100.7"),
		prefixes(t, "10.0.0.0/8"))
	if forwarded != "198.51.100.7" {
		t.Fatalf("the forwarded address keyed as %q, which is a budget of its own", forwarded)
	}
}

func TestAnUnparseablePeerIsNoAddress(t *testing.T) {
	t.Parallel()

	got := clientip.Of(request("@", ""), prefixes(t, "10.0.0.0/8"))
	if got.IsValid() {
		t.Fatalf("got %s, want the zero address", got)
	}
	if s := clientip.String(request("@", ""), nil); s != "" {
		t.Fatalf("String gave %q for an address that does not parse", s)
	}
}

func TestABareHostParses(t *testing.T) {
	t.Parallel()

	if got := clientip.Parse("198.51.100.7"); got.String() != "198.51.100.7" {
		t.Fatalf("got %s", got)
	}
}
