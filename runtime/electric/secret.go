// Keeping the sync service's credential out of what this package reports.
//
// The credential travels on the query string, because that is the only place the sync
// service reads it from. So the URL of every upstream request contains it, and an
// *url.Error — which is what a transport failure is — renders the URL it was given.
// net/url redacts a password in the userinfo of a URL and nothing else, so the credential
// survives into the string, into [Config.OnError], and into a log.
//
// That is why [Config.Secret] is a field of its own rather than an entry in
// [Config.Extra]: taking a value back out of what this package says requires knowing what
// the value is.
//
// A secret stated in [Config.Extra] is redacted too — see [credential]. The field is
// where one belongs, but the entry is where one had to go before the field existed, and a
// project that has not moved is the one already logging its credential.

package electric

import (
	"context"
	"net/url"
	"strings"
)

// redactedPlaceholder is what stands in for the secret. Not the empty string: a line with
// nothing where the credential was reads as though there never was one, and the point of
// saying anything is that somebody debugging a 502 can see the request was authorised.
const redactedPlaceholder = "[redacted]"

// credential is the value this package has to keep out of what it says, which is not
// quite the same question as which value it puts on a request.
//
// [Config.Secret] is where a secret belongs and is the only place [New] reads one from
// to send it. [Config.Extra] is where one had to go before that field existed, so a
// project still stating it there is the one most likely to be logging its credential
// today — and it reaches the upstream URL by exactly the same route. Redaction reads
// both, because taking a value back out of a string does not care where it was
// configured, and a rule that covered only the new spelling would leave the old one
// leaking with nothing to say so.
//
// [New] refuses the case where both are set, so there is never a second value to lose.
func credential(cfg Config) string {
	if cfg.Secret != "" {
		return cfg.Secret
	}
	return cfg.Extra.Get("secret")
}

// spellings are the forms of secret that can appear in a string this package reports.
//
// Two, because the URL inside an *url.Error is the encoded one: a secret containing
// anything outside the unreserved set appears percent-encoded there and verbatim nowhere.
// They are the same string for an alphanumeric secret, and deduplicating that case is not
// worth a comparison — replacing twice is idempotent.
func spellings(secret string) []string {
	if secret == "" {
		return nil
	}
	return []string{secret, url.QueryEscape(secret)}
}

// redact removes every spelling of secret from s.
func redact(s, secret string) string {
	for _, from := range spellings(secret) {
		s = strings.ReplaceAll(s, from, redactedPlaceholder)
	}
	return s
}

// A redactedError is an error whose message has the secret taken out of it.
//
// A wrapper rather than a rebuilt error, so that errors.Is and errors.As still reach what
// actually failed. A caller matching on context.DeadlineExceeded or on *url.Error is
// asking a question the redaction has no business answering.
type redactedError struct {
	err    error
	secret string
}

func (e *redactedError) Error() string { return redact(e.err.Error(), e.secret) }
func (e *redactedError) Unwrap() error { return e.err }

// redacting wraps err so its message cannot carry the secret. Nil in, nil out.
func redacting(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	return &redactedError{err: err, secret: secret}
}

// reporting wraps an error callback so that everything reaching it is redacted.
//
// Done once, at construction, rather than at each of the two sites that can carry the URL
// today. A rule about what may be logged is only worth having if the next error path
// cannot forget it, and a proxy grows error paths.
//
// Returns the callback unchanged when there is no secret, so a project without one pays
// nothing — not even an allocation per error.
func reporting(onError func(context.Context, error), secret string) func(context.Context, error) {
	if onError == nil || secret == "" {
		return onError
	}
	return func(ctx context.Context, err error) {
		onError(ctx, redacting(err, secret))
	}
}
