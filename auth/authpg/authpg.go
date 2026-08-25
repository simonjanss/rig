// Package authpg implements the auth stores over the tables
// `rig setup-project` creates.
//
// The auth packages take interfaces because an application might keep its
// accounts somewhere unexpected. Almost none do: they run the migrations rig
// wrote, against the schema rig designed, and then have to write four hundred
// lines of SQL to connect the two. This is that SQL, written once.
//
// If you changed the foundation schema, implement the interfaces yourself. If
// you did not, wiring authentication is [New] and nothing else.
package authpg

import (
	"context"
	"net/netip"

	"github.com/simonjanss/rig/runtime/dbx"
)

// Stores are the implementations, ready to hand to the auth packages.
type Stores struct {
	Accounts *AccountStore
	Sessions *SessionStore
	APIKeys  *APIKeyStore
	Log      *Log
}

// New builds every store over one connection.
//
// The connection is usually a pool. Where a transaction is needed the stores
// open one themselves, and a repository call made inside somebody else's
// transaction joins it — that is what [dbx.Conn] is for.
func New(db dbx.Beginner) *Stores {
	conn, ok := db.(dbx.Conn)
	if !ok {
		panic("authpg: the database must satisfy both dbx.Beginner and dbx.Conn; a pgxpool.Pool does")
	}
	return &Stores{
		Accounts: &AccountStore{db: conn, tx: db},
		Sessions: &SessionStore{db: conn, tx: db},
		APIKeys:  &APIKeyStore{db: conn, tx: db},
		Log:      &Log{db: conn},
	}
}

// conn returns the transaction on the context when there is one.
//
// Every read and write in this package goes through it, which is what lets a
// password reset write a credential and consume a link in one transaction
// without either method knowing it is in one.
func conn(ctx context.Context, fallback dbx.Conn) dbx.Conn {
	if tx, ok := dbx.Tx(ctx); ok {
		return tx
	}
	return fallback
}

// addr converts a stored address for a caller that wants a string.
func addrString(a *netip.Addr) string {
	if a == nil || !a.IsValid() {
		return ""
	}
	return a.String()
}

// addrValue converts a string for a column that wants an inet.
//
// An empty string is null rather than an error: an address is genuinely unknown
// for a request that arrived over a unix socket or through a test.
func addrValue(s string) *netip.Addr {
	if s == "" {
		return nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil
	}
	a = a.Unmap()
	return &a
}
