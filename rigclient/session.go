package rigclient

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/simonjanss/rig/runtime/authwire"
)

// ErrNoSession is returned when a session has nothing left to present: it was
// never signed in, or its refresh token has expired and the person has to sign
// in again.
var ErrNoSession = errors.New("rigclient: no session; sign in again")

// Session is a credential that keeps itself fresh.
//
// It holds the pair a sign-in returned and exchanges the refresh token before
// the access token expires, using the lifetimes in [AuthProfile] — so a program
// that runs for a day makes a handful of refresh calls at moments of its own
// choosing, rather than discovering the expiry through a failed request in the
// middle of something.
//
// It is safe for concurrent use, and a refresh is not: several goroutines
// arriving at once take turns, and the ones behind find the work already done
// instead of each spending a rotation. That matters more than it sounds — the
// server bounds how often a session may rotate, and a client that raced with
// itself would spend that budget on nothing.
type Session struct {
	mu     sync.Mutex
	tokens authwire.TokenPair
	rt     *Runtime
}

// NewSession builds a session around a pair a sign-in returned.
//
// The ordinary way to get one is [Auth.SignIn], which installs it. This is for a
// program that stored the pair somewhere between runs.
func NewSession(tokens authwire.TokenPair) *Session {
	return &Session{tokens: tokens}
}

// bind gives the session the client it refreshes through. Called when it is
// installed, which is the only moment it can be known.
func (s *Session) bind(rt *Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rt = rt
}

// Tokens returns the pair currently held, for a program that stores it between
// runs.
func (s *Session) Tokens() authwire.TokenPair {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.tokens
}

// SessionID identifies the session, for showing it in a list of the caller's own
// and revoking it.
func (s *Session) SessionID() string { return s.Tokens().SessionID.String() }

// replace swaps in a newly issued pair, which is what a refresh, a tenant
// switch and a password change all produce.
func (s *Session) replace(pair authwire.TokenPair) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A response that carried no new refresh token keeps the one in hand: some
	// endpoints answer with an access token alone, and dropping the refresh
	// token on one of those would end the session at the next expiry.
	if pair.RefreshToken == "" {
		pair.RefreshToken = s.tokens.RefreshToken
		pair.RefreshExpiresAt = s.tokens.RefreshExpiresAt
	}
	s.tokens = pair
}

// Apply implements [Credential]. It refreshes first if the token is close enough
// to expiry that this request might outlive it.
func (s *Session) Apply(ctx context.Context, r *http.Request) error {
	if s.stale() {
		if _, err := s.exchange(ctx, s.Tokens().AccessToken); err != nil {
			return err
		}
	}

	token := s.Tokens().AccessToken
	if token == "" {
		return ErrNoSession
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// Reauthorize implements [Reauthorizer]: a 401 despite a token that looked
// current is a revoked or invalidated one, and the refresh token is the only
// thing left to try.
//
// It exchanges whatever the expiry says, because the expiry is exactly what has
// just been proved wrong.
func (s *Session) Reauthorize(ctx context.Context) (bool, error) {
	return s.exchange(ctx, s.Tokens().AccessToken)
}

// stale reports whether the access token is expired or close enough to it.
func (s *Session) stale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rt == nil || s.tokens.AccessToken == "" {
		return false
	}
	if s.tokens.ExpiresAt.IsZero() {
		// A server that did not say when the token expires leaves nothing to
		// anticipate. The 401 retry is what covers that case.
		return false
	}
	leeway := s.rt.profile().refreshLeeway()
	return !s.rt.Now().Add(leeway).Before(s.tokens.ExpiresAt)
}

// exchange swaps the refresh token for a new pair, once, however many callers
// arrive at the same moment.
//
// previous is the access token the caller found wanting. Whoever reaches the
// lock first does the exchange; everybody behind them finds a different token
// in hand and returns without spending a second rotation — which matters
// because the server bounds how many a session gets.
func (s *Session) exchange(ctx context.Context, previous string) (bool, error) {
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()

	if rt == nil {
		return false, errors.New("rigclient: this session is not attached to a client")
	}
	auth := rt.Auth()
	if auth == nil {
		return false, errors.New("rigclient: this API has no authentication endpoints to refresh at")
	}

	rt.refreshing.Lock()
	defer rt.refreshing.Unlock()

	tokens := s.Tokens()
	if tokens.AccessToken != previous && tokens.AccessToken != "" {
		return true, nil
	}
	if tokens.RefreshToken == "" {
		return false, ErrNoSession
	}

	pair, err := auth.Refresh(ctx, tokens.RefreshToken)
	if err != nil {
		return false, err
	}
	s.replace(*pair)
	return true, nil
}

// Expired reports whether the session has nothing usable left: the refresh
// token's own lifetime has run out, and only a fresh sign-in will do.
func (s *Session) Expired(now time.Time) bool {
	tokens := s.Tokens()
	if tokens.RefreshToken == "" {
		return true
	}
	return !tokens.RefreshExpiresAt.IsZero() && !now.Before(tokens.RefreshExpiresAt)
}
