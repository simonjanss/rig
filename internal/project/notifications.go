package project

import (
	"slices"
	"time"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// The digest windows an account can ask for, and the one that is not a window.
const (
	// DigestImmediate sends each delivery on its own, as soon as it is due.
	DigestImmediate = "Immediate"
	DigestHourly    = "Hourly"
	DigestDaily     = "Daily"
	DigestWeekly    = "Weekly"
	// DigestOff sends nothing on that channel and still writes the inbox line.
	// It is not the same as turning the channel off, and both exist: `Off` is
	// about this account's preference, `is_enabled: false` is about the channel.
	DigestOff = "Off"
)

// Digests is every accepted value, for the schema and for the diagnostic.
func Digests() []string {
	return []string{DigestImmediate, DigestHourly, DigestDaily, DigestWeekly, DigestOff}
}

// Notifications is the inbox and the machinery that fills it.
//
// It is configuration rather than a Go literal for the reason `auth:` and
// `files:` are: a claim lease or a retention window written in code is a number
// the generated documentation cannot quote and nobody can find without reading
// the wiring. Everything with a fixed answer is here, resolved when the file is
// read.
//
// Zero means "left out" throughout, and there is deliberately no way to write a
// zero that survives: a claim lease of nothing and a retention of nothing are
// not configurations anybody wants.
type Notifications struct {
	// Enabled says this project has an inbox. Off by default, and what makes
	// `server-go` write the wiring and the compiler keep rig's two notification
	// tables in the schema at all.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether this project has an inbox. The server-go generator writes the wiring only when it is set."`

	// Expose projects rig_notification_recipient as a resource, so the inbox
	// gets the filter grammar, the sort keys and a generated client.
	//
	// Off is the ordinary case and still leaves a working inbox: the routes
	// under /notifications are hand-written, because the tables are the same in
	// every project and there is nothing for a generator to vary. This is for
	// the project those routes are not enough for — and both stay, because the
	// difference between them is the point.
	//
	// It does not decide whether the tables are in the schema. They are, as soon
	// as `enabled` is set, because a link table pointing at a table rig has
	// dropped is not a link table any more.
	Expose bool `yaml:"expose,omitempty" json:"expose,omitempty" jsonschema_description:"Project the inbox as a generated resource as well, for the filter grammar and a typed client. The hand-written routes exist either way."`

	// DefaultDigest is what an account gets on a channel it has never set a
	// preference for. The last step of the three-step resolution.
	DefaultDigest string `yaml:"default_digest,omitempty" json:"default_digest,omitempty" jsonschema:"enum=Immediate,enum=Hourly,enum=Daily,enum=Weekly,enum=Off" jsonschema_description:"What an account gets on a channel it has no setting for. Defaults to Immediate."`

	// ClaimTTL is how long a dispatcher's claim on a delivery is honoured before
	// another one may take it.
	//
	// It exists for crashes: a process that claimed a row and died leaves it
	// Pending with a stale claim, and the next dispatcher past it takes it once
	// this has elapsed. A clean shutdown gives its claims back immediately and
	// does not wait this out.
	//
	// The number that matters is the slowest channel's own timeout, which rig
	// cannot know. Shorter than that and every message is claimed twice under
	// ordinary load — at-least-once stops being a crash-recovery property and
	// becomes an everyday one. Longer and a crashed pod's queue sits idle for
	// that long while healthy replicas walk past it.
	ClaimTTL Duration `yaml:"claim_ttl,omitempty" json:"claim_ttl,omitempty" jsonschema_description:"How long a dispatcher's claim on a delivery is honoured before another may take it. Set it longer than your slowest channel's timeout. Defaults to 5m, and under 1m is refused."`

	// SendTimeout bounds one call into a channel.
	//
	// It is the deadline on the context a channel is handed, and the answer to
	// the relationship ClaimTTL's own comment above calls the one
	// misconfiguration worth understanding: rig could not know the slowest
	// channel's timeout while the channel owned it, and now the channel is told
	// what it is. Shorter than ClaimTTL, necessarily — a send that may outlive
	// the lease protecting it is a send whose row somebody else has already
	// taken.
	//
	// A channel is the only outbound call rig does not make itself, which is why
	// this is the only such value in this file. It is also cooperative: a Sender
	// that ignores the deadline hangs its dispatcher anyway, and the interface
	// documentation says so.
	SendTimeout Duration `yaml:"send_timeout,omitempty" json:"send_timeout,omitempty" jsonschema_description:"How long one call into a channel may take before it is treated as failed. Has to be shorter than claim_ttl. Defaults to 30s, and under 1s is refused."`

	// MaxAttempts is how many times a delivery is tried before it is Failed and
	// stops being claimed.
	MaxAttempts int `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty" jsonschema_description:"How many times a delivery is tried before it is marked failed. Defaults to 14, which with the default base and cap spans about eight hours."`

	// BackoffBase is the first retry delay. Each attempt doubles it, up to
	// BackoffCap.
	BackoffBase Duration `yaml:"backoff_base,omitempty" json:"backoff_base,omitempty" jsonschema_description:"Delay before the first retry. Each attempt doubles it, up to backoff_cap. Defaults to 1m."`

	// BackoffCap is the longest one wait may be, which is what makes a long
	// schedule safe to have.
	//
	// The three retry numbers are one decision, and this is the one that decides
	// what the tail of the schedule looks like. Doubling from a minute fourteen
	// times is five days, so without a ceiling the late attempts are not retries
	// — they are a row nobody will look at again. Capped, the tail is a series of
	// hourly knocks, which is what somebody watching a provider's status page
	// would do by hand.
	BackoffCap Duration `yaml:"backoff_cap,omitempty" json:"backoff_cap,omitempty" jsonschema_description:"The longest one retry delay may be, once the doubling reaches it. Has to be at least backoff_base. Defaults to 1h."`

	// Retention is how long a read and deleted inbox line is kept before the
	// dispatch task prunes it, along with the notifications and link rows left
	// with nothing pointing at them.
	//
	// It has to outlive the longest digest window: a weekly digest under a daily
	// retention is a digest assembled from rows that were deleted before it ran,
	// and it would present as "the weekly mail is sometimes empty" rather than
	// as a configuration error.
	Retention Duration `yaml:"retention,omitempty" json:"retention,omitempty" jsonschema_description:"How long a read and deleted inbox line is kept before it is pruned. Has to outlive the longest digest window. Defaults to 2160h (90 days)."`
}

// MinClaimTTL is the shortest lease rig will start with.
//
// A lease shorter than this is a lease that expires while an ordinary HTTP call
// to a mail provider is still in flight, which makes every message claimed twice
// under normal load rather than only after a crash. Refused at boot rather than
// discovered as duplicate mail.
const MinClaimTTL = time.Minute

// MinSendTimeout is the shortest send timeout the document can carry, which is
// the only reason there is one: every duration in this block is resolved to whole
// seconds for the IR, so anything under a second arrives as zero and zero reads
// as unset.
const MinSendTimeout = time.Second

// LongestDigestWindow is the widest window a digest can wait for, and therefore
// the floor under [Notifications.Retention].
const LongestDigestWindow = 7 * 24 * time.Hour

// Configured reports whether the notifications block says anything beyond
// whether there is an inbox and how its tables are treated, the same way
// [Files.Configured] does.
func (n Notifications) Configured() bool {
	return configured(n, Notifications{Enabled: n.Enabled, Expose: n.Expose})
}

// checkNotifications validates what the JSON Schema cannot: values that only
// make sense relative to each other, and a block nothing reads.
func (p *Project) checkNotifications() diag.List {
	var diags diag.List
	n := p.Config.Notifications

	if !n.Enabled {
		// The same failure mode the auth and files blocks have, and refused for
		// the same reason: a retention somebody set and believed in, unread.
		if n.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("notifications", "enabled"),
				"notifications is configured but notifications.enabled is false, so none of it "+
					"is read; set `enabled: true` or remove the block")
		}
		return diags
	}

	if !slices.Contains(Digests(), n.DefaultDigest) {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "default_digest"),
			"notifications.default_digest is %q; it has to be one of %v", n.DefaultDigest, Digests())
	}

	// The one misconfiguration worth understanding before deploying this. It is
	// refused at boot rather than at dispatch time for the reason the S3 adapter
	// refuses a bucket lifecycle shorter than the restore window: the failure it
	// prevents does not look like a failure, it looks like duplicate mail.
	if ttl := n.ClaimTTL.Duration(); ttl > 0 && ttl < MinClaimTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "claim_ttl"),
			"notifications.claim_ttl is %s, and under %s every message a slow channel is still "+
				"sending is claimed a second time — at-least-once stops being about crashes and "+
				"becomes ordinary load. Set it longer than your slowest channel's own timeout",
			n.ClaimTTL, MinClaimTTL)
	}

	// The same misconfiguration as the one above, stated as arithmetic rather
	// than as advice. `claim_ttl`'s own documentation says to set it longer than
	// the slowest channel's timeout; with that timeout a key here, the pair can
	// be refused instead of explained. Both values are fine alone, which is what
	// makes this a check and not a range — the shape the retention and digest
	// window check below has.
	if ttl, send := n.ClaimTTL.Duration(), n.SendTimeout.Duration(); ttl > 0 && send > 0 && send >= ttl {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "send_timeout"),
			"notifications.send_timeout is %s and notifications.claim_ttl is %s, so a channel may "+
				"still be sending when the lease protecting its row expires and another dispatcher "+
				"takes it — every slow message would be sent twice. Set send_timeout below claim_ttl",
			n.SendTimeout, n.ClaimTTL)
	}

	// A floor, because the document carries this in seconds. `500ms` is a value
	// somebody could reasonably want for a webhook and there is no room for it
	// here: it resolves to zero, the generated wiring passes zero, and rig/notify
	// reads zero as "unset" and uses thirty seconds — sixty times what was asked
	// for, quietly. Refused rather than rounded, because the two are
	// indistinguishable afterwards.
	if send := n.SendTimeout.Duration(); send > 0 && send < MinSendTimeout {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "send_timeout"),
			"notifications.send_timeout is %s, and under %s cannot be carried: the value is "+
				"resolved in seconds and would arrive as none at all, which reads as unset and "+
				"becomes the %s default", n.SendTimeout, MinSendTimeout, DefaultNotificationSendTimeout)
	}

	if n.MaxAttempts < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "max_attempts"),
			"notifications.max_attempts is %d; a negative count sends nothing", n.MaxAttempts)
	}

	// The third pair in this block that cannot both be true, and refused for the
	// reason the first two are: a ceiling under the floor means the doubling never
	// happens, so every retry waits the cap and nothing backoff_base's
	// documentation says is true of this project. It reads as an exponential
	// schedule and behaves as a fixed one, which is the kind of disagreement that
	// is only ever found by wondering why the retries are evenly spaced.
	base, ceiling := n.BackoffBase.Duration(), n.BackoffCap.Duration()
	if base > 0 && ceiling > 0 && ceiling < base {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "backoff_cap"),
			"notifications.backoff_cap is %s and backoff_base is %s, so the cap binds before "+
				"the first doubling and every retry waits the same %s; set backoff_cap above "+
				"backoff_base, or remove it for the %s default",
			n.BackoffCap, n.BackoffBase, n.BackoffCap, DefaultBackoffCap)
	}

	// A weekly digest under a daily retention assembles itself from rows that
	// were pruned before it ran, and presents as "the weekly mail is sometimes
	// empty" rather than as a configuration error.
	if r := n.Retention.Duration(); r > 0 && r < LongestDigestWindow {
		diags.Add(diag.CodeConfigInvalid, p.At("notifications", "retention"),
			"notifications.retention is %s, which is shorter than the longest digest window (%s); "+
				"a weekly digest would be assembled from rows that had already been pruned",
			n.Retention, LongestDigestWindow)
	}

	// What an inbox needs is the tenancy tables, not the `auth:` block: a
	// notification is addressed to an account, and an application whose claims
	// come from a header has accounts like any other. Whether those tables are
	// there is a question about migrations rather than about configuration, and
	// it is asked where the migrations are — see checkFoundationBlock.

	return diags
}

// IR is the resolved block, as a document carries it.
//
// Nil for a project with no inbox, so that a generator asks the document one
// question — are there notifications — rather than reading a flag and then five
// numbers that may or may not mean anything.
//
// Every duration is resolved to seconds here. A document is JSON, and a Go
// duration in one is either a number nobody can read or a string every consumer
// has to parse.
func (n Notifications) IR() *ir.Notifications {
	if !n.Enabled {
		return nil
	}

	return &ir.Notifications{
		Enabled:            true,
		Expose:             n.Expose,
		DefaultDigest:      n.DefaultDigest,
		ClaimTTLSeconds:    int64(n.ClaimTTL.Duration().Seconds()),
		SendTimeoutSeconds: int64(n.SendTimeout.Duration().Seconds()),
		MaxAttempts:        n.MaxAttempts,
		BackoffBaseSeconds: int64(n.BackoffBase.Duration().Seconds()),
		BackoffCapSeconds:  int64(n.BackoffCap.Duration().Seconds()),
		RetentionSeconds:   int64(n.Retention.Duration().Seconds()),
	}
}
