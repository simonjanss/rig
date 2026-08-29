// Package clientip answers "where did this request come from".
//
// It is one short function, in a package of its own, because getting it wrong
// is silent. A server that believes X-Forwarded-For unconditionally lets any
// caller name their own address, and every limit keyed on one becomes
// decorative — the requests still arrive, the counter still counts, and it
// counts a different fictional client each time. A server that never believes
// it sees only the load balancer, so one address carries everybody and the
// limit is either off or on for the whole internet.
//
// The answer is neither: believe the header exactly when the immediate peer is
// a proxy the application named. That has to be the same answer everywhere it
// is asked, which is why this is not a method on whichever handler needed it
// first.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Of returns the address the request came from.
//
// trusted are the networks whose X-Forwarded-For may be believed. Empty trusts
// none of them, which is the right default and the wrong setting behind a load
// balancer — see [Parse] for the one that is not a choice.
//
// The zero [netip.Addr] is a peer that could not be parsed at all, which
// happens on a Unix socket and in tests. Callers should treat it as "no
// address" rather than as one, since every request with no address would
// otherwise share a budget.
func Of(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := Parse(r.RemoteAddr)

	if len(trusted) == 0 || !peer.IsValid() {
		return peer
	}
	if !contains(trusted, peer) {
		return peer
	}

	// The left-most entry is the original client. Everything after it was added
	// by a hop, and everything before the first untrusted hop is a claim.
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return peer
	}
	first, _, _ := strings.Cut(forwarded, ",")
	if addr, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
		// Unmapped for the same reason [Parse] unmaps the peer: a proxy that
		// writes ::ffff:198.51.100.7 where another writes 198.51.100.7 is
		// naming one client, and two spellings of one address are two budgets.
		return addr.Unmap()
	}
	return peer
}

// String is [Of] rendered for a log or a rate-limit key, and "" for no address.
func String(r *http.Request, trusted []netip.Prefix) string {
	if a := Of(r, trusted); a.IsValid() {
		return a.String()
	}
	return ""
}

// Parse reads an address out of a host:port, or out of a bare host.
//
// IPv4-in-IPv6 is unmapped, so that ::ffff:198.51.100.7 and 198.51.100.7 are
// one address rather than two budgets.
func Parse(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
