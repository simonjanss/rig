package project_test

import (
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/project"
)

// parseNotifications is the whole loop, the way parseFiles is: text in, resolved
// configuration and diagnostics out. Through Parse rather than around it,
// because the defaults are applied there and the pair of values this file cares
// about is only a pair once they have been.
func parseNotifications(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	src := "project:\n  name: demo\n  module: example.com/demo\n" + body
	p, diags := project.Parse("rig.yaml", []byte(src))
	return p, diags.String()
}

func TestNotificationDefaults(t *testing.T) {
	p, out := parseNotifications(t, "notifications:\n  enabled: true\n")
	if out != "" {
		t.Fatalf("a bare enabled block should be valid:\n%s", out)
	}

	n := p.Config.Notifications
	if n.ClaimTTL.Duration() != project.DefaultClaimTTL {
		t.Errorf("claim_ttl = %s, want %s", n.ClaimTTL, project.DefaultClaimTTL)
	}
	if n.SendTimeout.Duration() != project.DefaultNotificationSendTimeout {
		t.Errorf("send_timeout = %s, want %s", n.SendTimeout, project.DefaultNotificationSendTimeout)
	}
	if n.MaxAttempts != project.DefaultMaxAttempts {
		t.Errorf("max_attempts = %d, want %d", n.MaxAttempts, project.DefaultMaxAttempts)
	}
	if n.BackoffBase.Duration() != project.DefaultBackoffBase {
		t.Errorf("backoff_base = %s, want %s", n.BackoffBase, project.DefaultBackoffBase)
	}
	if n.BackoffCap.Duration() != project.DefaultBackoffCap {
		t.Errorf("backoff_cap = %s, want %s", n.BackoffCap, project.DefaultBackoffCap)
	}
}

// The three retry numbers are one decision, and the thing worth pinning is what
// they add up to: an outage is measured in hours by everyone who has had one, and
// the old five-attempt schedule spanned thirty-one minutes. If somebody changes
// one of the three, this is the test that says what it cost.
func TestTheDefaultRetryScheduleIsMeasuredInHours(t *testing.T) {
	t.Parallel()

	base, ceiling := project.DefaultBackoffBase, project.DefaultBackoffCap

	// The same arithmetic notify does, and one wait short of max_attempts because
	// the last failure is a failure rather than a wait.
	var total time.Duration
	for attempt := 1; attempt < project.DefaultMaxAttempts; attempt++ {
		wait := base
		for range attempt - 1 {
			if wait >= ceiling {
				break
			}
			wait *= 2
		}
		total += min(wait, ceiling)
	}

	if want := 8*time.Hour + 3*time.Minute; total != want {
		t.Errorf("the default schedule spans %s, want %s — the three retry defaults "+
			"no longer add up to the window the documentation quotes", total, want)
	}
}

// A ceiling under the floor is a schedule that reads as exponential and behaves
// as fixed, so it is refused where somebody can still change one of the two
// numbers — the third pair in this block that cannot both be true.
func TestABackoffCapBelowTheBaseIsRefused(t *testing.T) {
	t.Parallel()

	_, out := parseNotifications(t,
		"notifications:\n  enabled: true\n  backoff_base: 5m\n  backoff_cap: 1m\n")
	if out == "" {
		t.Fatal("a backoff_cap below backoff_base was accepted")
	}
	// Both numbers and both keys, because the fix is to change one of them and
	// the message should not make the reader work out which two disagreed.
	for _, want := range []string{"backoff_cap", "backoff_base", "5m", "1m"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic does not mention %q:\n%s", want, out)
		}
	}
}

// Equal is allowed, and that is deliberate rather than an oversight: a cap equal
// to the base is a flat schedule somebody asked for in so many words, which is a
// different thing from one they arrived at by setting two numbers the wrong way
// round.
func TestABackoffCapEqualToTheBaseIsAllowed(t *testing.T) {
	t.Parallel()

	if _, out := parseNotifications(t,
		"notifications:\n  enabled: true\n  backoff_base: 1m\n  backoff_cap: 1m\n"); out != "" {
		t.Errorf("a flat schedule stated outright was refused:\n%s", out)
	}
}

// The defaults have to satisfy the rule the next test enforces. A pair of
// defaults that the validator would refuse is a project that cannot start
// without configuring its way out of rig's own numbers.
func TestTheDefaultSendTimeoutFitsTheDefaultLease(t *testing.T) {
	if project.DefaultNotificationSendTimeout >= project.DefaultClaimTTL {
		t.Errorf("send_timeout default %s does not fit inside claim_ttl default %s, so the "+
			"shipped configuration is one this package refuses",
			project.DefaultNotificationSendTimeout, project.DefaultClaimTTL)
	}
}

// Both numbers are fine alone, which is what makes this a relationship rather
// than a range — the same shape as the retention and digest-window check.
func TestASendMayNotOutliveItsOwnLease(t *testing.T) {
	_, out := parseNotifications(t, "notifications:\n  enabled: true\n"+
		"  claim_ttl: 2m\n  send_timeout: 5m\n")
	if !strings.Contains(out, "send_timeout") {
		t.Errorf("a send_timeout above claim_ttl was accepted:\n%s", out)
	}

	// Equal is refused with longer. A send that ends exactly when its lease does
	// ends after it in practice, because the lease was stamped before the send
	// started — and both the diagnostic and the schema description say "below".
	_, equal := parseNotifications(t, "notifications:\n  enabled: true\n"+
		"  claim_ttl: 5m\n  send_timeout: 5m\n")
	if !strings.Contains(equal, "send_timeout") {
		t.Errorf("a send_timeout equal to claim_ttl was accepted:\n%s", equal)
	}

	// And the inverse in the same test, because a check that refused every pair
	// would pass the assertions above while making the feature unusable.
	_, ok := parseNotifications(t, "notifications:\n  enabled: true\n"+
		"  claim_ttl: 5m\n  send_timeout: 30s\n")
	if ok != "" {
		t.Errorf("an ordinary pair was refused:\n%s", ok)
	}
}

// A value the document cannot carry is refused rather than rounded to zero,
// which the engine would read as unset and answer with thirty seconds — sixty
// times what was written, and nothing to say so.
func TestASubSecondSendTimeoutIsRefused(t *testing.T) {
	_, out := parseNotifications(t, "notifications:\n  enabled: true\n  send_timeout: 500ms\n")
	if !strings.Contains(out, "send_timeout") {
		t.Errorf("a send_timeout of 500ms was accepted, and it resolves to zero:\n%s", out)
	}

	// The floor itself is fine, so the check is a floor and not a ban on small
	// numbers.
	_, ok := parseNotifications(t, "notifications:\n  enabled: true\n  send_timeout: 1s\n")
	if ok != "" {
		t.Errorf("a send_timeout of exactly the minimum was refused:\n%s", ok)
	}
}

// The IR is what the generator reads, and a value resolved but not carried is a
// value the generated wiring does not pass.
func TestTheSendTimeoutReachesTheDocument(t *testing.T) {
	p, out := parseNotifications(t, "notifications:\n  enabled: true\n  send_timeout: 45s\n")
	if out != "" {
		t.Fatalf("this configuration should be accepted:\n%s", out)
	}

	got := p.Config.Notifications.IR()
	if got == nil {
		t.Fatal("an enabled block produced no IR")
	}
	if got.SendTimeoutSeconds != 45 {
		t.Errorf("send_timeout_seconds = %d, want 45", got.SendTimeoutSeconds)
	}
}

// Nothing is resolved for a project that never turned the block on, the way
// TestFilesDefaultsAreNotAppliedWhenOff asserts for files: an unfinished block
// should read as unfinished.
func TestNotificationDefaultsAreNotAppliedWhenOff(t *testing.T) {
	p, out := parseNotifications(t, "")
	if out != "" {
		t.Fatalf("no notifications block at all should be valid:\n%s", out)
	}
	if p.Config.Notifications.SendTimeout.Duration() != 0 {
		t.Errorf("a project with no notifications block got a send_timeout anyway: %s",
			p.Config.Notifications.SendTimeout)
	}
}
