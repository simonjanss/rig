package project_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

// parsePresence is the whole loop, the way parseNotifications is: text in,
// resolved configuration and diagnostics out. Through Parse rather than around
// it, because the defaults are applied there and the pairs of values this file
// cares about are only pairs once they have been.
func parsePresence(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	src := "project:\n  name: demo\n  module: example.com/demo\n" + body
	p, diags := project.Parse("rig.yaml", []byte(src))
	return p, diags.String()
}

func TestPresenceDefaults(t *testing.T) {
	p, out := parsePresence(t, "presence:\n  enabled: true\n")
	if out != "" {
		t.Fatalf("a bare enabled block should be valid:\n%s", out)
	}

	c := p.Config.Presence
	if c.TTL.Duration() != project.DefaultPresenceTTL {
		t.Errorf("ttl = %s, want %s", c.TTL, project.DefaultPresenceTTL)
	}
	if c.Heartbeat.Duration() != project.DefaultPresenceHeartbeat {
		t.Errorf("heartbeat = %s, want %s", c.Heartbeat, project.DefaultPresenceHeartbeat)
	}
	if c.Sweep.Duration() != project.DefaultPresenceSweep {
		t.Errorf("sweep = %s, want %s", c.Sweep, project.DefaultPresenceSweep)
	}
	if c.Grace.Duration() != project.DefaultPresenceGrace {
		t.Errorf("grace = %s, want %s", c.Grace, project.DefaultPresenceGrace)
	}
}

// The defaults have to satisfy the rule the checks enforce. A pair of defaults
// this package refuses is a project that cannot start without configuring its way
// out of rig's own numbers.
func TestTheShippedPresenceDefaultsAreAccepted(t *testing.T) {
	if project.DefaultPresenceTTL < project.PresenceBeatsBeforeGone*project.DefaultPresenceHeartbeat {
		t.Errorf("the default ttl %s is under %d default heartbeats (%s), so the shipped "+
			"configuration is one this package refuses",
			project.DefaultPresenceTTL, project.PresenceBeatsBeforeGone,
			project.PresenceBeatsBeforeGone*project.DefaultPresenceHeartbeat)
	}
	if project.DefaultPresenceTTL < project.MinPresenceTTL {
		t.Errorf("the default ttl %s is under the minimum %s",
			project.DefaultPresenceTTL, project.MinPresenceTTL)
	}
}

// A block nothing reads is refused, the way the auth, files and notifications
// blocks are: a TTL somebody set and believed in, unread, is the failure this
// prevents.
func TestPresenceConfiguredButNotEnabled(t *testing.T) {
	_, out := parsePresence(t, "presence:\n  ttl: 5m\n")
	if !strings.Contains(out, "presence.enabled") {
		t.Errorf("a configured block with no enabled was accepted:\n%s", out)
	}

	// And `expose` alone is not "configured": it is about how the table is
	// treated rather than about how presence behaves, so it must not trip this.
	_, bare := parsePresence(t, "presence:\n  expose: true\n")
	if strings.Contains(bare, "presence.enabled") {
		t.Errorf("expose alone was treated as configuration:\n%s", bare)
	}
}

// Three beats is the floor, and it is a relationship rather than a range: both
// numbers below are fine alone.
func TestATTLHasToOutlastThreeBeats(t *testing.T) {
	_, out := parsePresence(t, "presence:\n  enabled: true\n  heartbeat: 30s\n  ttl: 60s\n")
	if !strings.Contains(out, "presence.ttl") {
		t.Errorf("a ttl under three heartbeats was accepted:\n%s", out)
	}

	// The inverse, because a check that refused every pair would pass the
	// assertion above while making the feature unusable.
	_, ok := parsePresence(t, "presence:\n  enabled: true\n  heartbeat: 20s\n  ttl: 60s\n")
	if ok != "" {
		t.Errorf("three beats exactly was refused:\n%s", ok)
	}
}

func TestATTLUnderTheMinimumIsRefused(t *testing.T) {
	_, out := parsePresence(t, "presence:\n  enabled: true\n  ttl: 5s\n  heartbeat: 1s\n")
	if !strings.Contains(out, "presence.ttl") {
		t.Errorf("a ttl of 5s was accepted:\n%s", out)
	}
}

// A sub-second heartbeat resolves to zero in the document, which reads as unset
// and silently becomes the default — forty times what was asked for. Refused
// rather than rounded, because the two are indistinguishable afterwards.
func TestASubSecondHeartbeatIsRefusedRatherThanRounded(t *testing.T) {
	_, out := parsePresence(t, "presence:\n  enabled: true\n  heartbeat: 500ms\n")
	if !strings.Contains(out, "presence.heartbeat") {
		t.Errorf("a heartbeat of 500ms was accepted:\n%s", out)
	}
}

// A sweep faster than the TTL works and is wasteful, so it warns rather than
// refuses — the one rule in this block that is not a hard no.
func TestAFastSweepWarnsRatherThanRefuses(t *testing.T) {
	src := "project:\n  name: demo\n  module: example.com/demo\n" +
		"presence:\n  enabled: true\n  sweep: 10s\n"
	_, diags := project.Parse("rig.yaml", []byte(src))

	if !strings.Contains(diags.String(), "presence.sweep") {
		t.Fatalf("a sweep faster than the ttl said nothing:\n%s", diags)
	}
	// The severity from the list rather than from the rendered text: the hint
	// every diagnostic carries ends in "inline errors", so a substring check for
	// "error" hits on a clean warning.
	if diags.HasErrors() {
		t.Errorf("a fast sweep was refused rather than warned about:\n%s", diags)
	}
}

// There is no value here that turns the sweeper off, and this is the test that
// says so on purpose rather than by omission.
//
// A negative duration was the intended way to say it and cannot be written: the
// duration pattern admits no sign. That is the right answer — whether a process
// runs a background loop is a line in its own main function, the way it is for
// the notification engine — and this test is what stops somebody adding a
// `sweep: -1s` escape hatch that the schema would reject in an editor and accept
// nowhere.
func TestASweepCannotBeTurnedOffFromTheConfiguration(t *testing.T) {
	_, out := parsePresence(t, "presence:\n  enabled: true\n  sweep: -1s\n")
	if out == "" {
		t.Fatal("a negative sweep was accepted; if that is now intended, the key's " +
			"documentation and applyPresenceDefaults have to say so too")
	}
}

// The IR is nil for a project without presence, so a generator asks the document
// one question rather than reading a flag and then four numbers.
func TestPresenceIRIsNilWhenDisabled(t *testing.T) {
	if got := (project.Presence{}).IR(); got != nil {
		t.Errorf("a disabled block produced %+v, want nil", got)
	}
}

func TestPresenceIRResolvesToSeconds(t *testing.T) {
	p, out := parsePresence(t, "presence:\n  enabled: true\n  ttl: 90s\n  heartbeat: 30s\n"+
		"  sweep: 2m\n  grace: 10m\n")
	if out != "" {
		t.Fatalf("unexpected diagnostics:\n%s", out)
	}

	got := p.Config.Presence.IR()
	if got == nil {
		t.Fatal("an enabled block produced no IR")
	}
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"ttl", got.TTLSeconds, 90},
		{"heartbeat", got.HeartbeatSeconds, 30},
		{"sweep", got.SweepSeconds, 120},
		{"grace", got.GraceSeconds, 600},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d seconds, want %d", tc.name, tc.got, tc.want)
		}
	}
}
