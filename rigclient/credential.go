package rigclient

import (
	"context"
	"net/http"
)

// Credential is what authorizes a request.
//
// One method, called once per request, which is what lets a credential that
// expires do its work without the call sites knowing: [Session] refreshes here,
// and everything above it goes on calling methods.
type Credential interface {
	// Apply adds whatever the request needs — an Authorization header, in every
	// implementation here. It may block, and it may fail, and a failure means
	// the request is not sent.
	Apply(ctx context.Context, r *http.Request) error
}

// Reauthorizer is a credential that might be able to recover from a refusal.
//
// The transport calls it once, and only once, when a request comes back 401. A
// static token reports nothing to try; a session exchanges its refresh token.
type Reauthorizer interface {
	// Reauthorize reports whether it did something worth retrying for.
	Reauthorize(ctx context.Context) (bool, error)
}

// StaticToken presents a token that does not change: an access token from
// somewhere else, or one issued for a script that runs for less time than the
// token lasts.
type StaticToken string

// Apply implements [Credential].
func (t StaticToken) Apply(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+string(t))
	return nil
}

// APIKey presents an API key secret — the rig_sk_ value shown once, when the key
// was minted.
//
// It is a distinct type from [StaticToken] although the header is identical, so
// that a caller reading the code can see which kind of credential a program
// holds. The server tells them apart by prefix.
type APIKey string

// Apply implements [Credential].
func (k APIKey) Apply(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+string(k))
	return nil
}
