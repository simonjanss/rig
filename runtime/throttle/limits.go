package throttle

import "time"

// The kinds of key the standard limits count against.
const (
	KeyEmailAddress = "email_address"
	KeyIPAddress    = "ip_address"
	KeyAccount      = "account"
	KeyAPIKey       = "api_key"
	KeyTokenFamily  = "token_family"
)

// The events the standard limits count. They match the auth log's event names,
// which is what lets the log double as the rate-limit substrate.
const (
	EventLoginFailed          = "LoginFailed"
	EventLoginSucceeded       = "LoginSucceeded"
	EventPasswordResetRequest = "PasswordResetRequested"
	EventVerificationResent   = "VerificationResent"
	EventTokenRefreshed       = "TokenRefreshed"
	EventAPIKeyAuthFailed     = "ApiKeyAuthFailed"
	EventAPIKeyAuthSucceeded  = "ApiKeyAuthSucceeded"
)

// Defaults are the limits a project starts with.
//
// The login pair is the reason this package exists in the shape it does. An
// email-only limit lets one attacker lock a victim out of their own account by
// failing on purpose; an IP-only limit lets a botnet spray one password across
// every address it knows. So there are two, deliberately mismatched: the email
// threshold is tight, and the IP threshold is loose enough that an office
// behind one NAT does not trip it on a Monday morning.
type Defaults struct {
	// LoginByEmail locks one account after a handful of wrong passwords.
	LoginByEmail Limit
	// LoginByIP throttles one source spraying many accounts.
	LoginByIP Limit

	PasswordReset      Limit
	VerificationResend Limit

	// Refresh bounds one session's rotations. A client refreshing sixty times
	// a minute is looping, not working.
	Refresh Limit

	APIKeyFailures Limit
}

// Standard returns the default limits.
func Standard() Defaults {
	return Defaults{
		LoginByEmail: Limit{
			Name:      "login.email",
			Event:     EventLoginFailed,
			ClearedBy: EventLoginSucceeded,
			Max:       5,
			Window:    15 * time.Minute,
		},
		LoginByIP: Limit{
			Name:  "login.ip",
			Event: EventLoginFailed,
			// Deliberately not cleared by a success. One valid login from a
			// shared address would otherwise wipe the record of a thousand
			// failures from the same place, which is the whole thing this
			// limit exists to notice.
			Max:    50,
			Window: 15 * time.Minute,
		},
		PasswordReset: Limit{
			Name:   "password.reset",
			Event:  EventPasswordResetRequest,
			Max:    5,
			Window: time.Hour,
		},
		VerificationResend: Limit{
			Name:   "verification.resend",
			Event:  EventVerificationResent,
			Max:    5,
			Window: time.Hour,
		},
		Refresh: Limit{
			Name: "token.refresh",
			// The counted event is a success, not a failure: this bounds how
			// hard one session may hammer the endpoint, and nothing clears it.
			Event:  EventTokenRefreshed,
			Max:    60,
			Window: time.Minute,
		},
		APIKeyFailures: Limit{
			Name:      "apikey.failed",
			Event:     EventAPIKeyAuthFailed,
			ClearedBy: EventAPIKeyAuthSucceeded,
			Max:       20,
			Window:    time.Minute,
		},
	}
}

// Email builds a key from an address.
//
// The address is not normalized here: the caller has already lowercased it to
// look the account up, and normalizing in two places is how the limit ends up
// counting "Sam@example.com" separately from "sam@example.com".
func Email(lowercased string) Key { return Key{Kind: KeyEmailAddress, Value: lowercased} }

// IP builds a key from a remote address.
func IP(addr string) Key { return Key{Kind: KeyIPAddress, Value: addr} }

// Account builds a key from an account identifier.
func Account(id string) Key { return Key{Kind: KeyAccount, Value: id} }

// APIKey builds a key from an API key's public identifier.
func APIKey(keyID string) Key { return Key{Kind: KeyAPIKey, Value: keyID} }

// TokenFamily builds a key from a session's root token.
func TokenFamily(rootID string) Key { return Key{Kind: KeyTokenFamily, Value: rootID} }
