package project_test

import (
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/project"
)

// authOn is what every valid `cache:` block needs beside it. The two reads rig
// caches are both authentication's, so the block is only read by a project that
// has one.
const authOn = "auth:\n  enabled: true\n"

// parseCache parses a configuration that may be invalid and returns both
// halves: most of what is worth checking here is what gets refused.
func parseCache(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	p, diags := project.Parse("rig.yaml", []byte(minimal+body))
	return p, diags.String()
}

// Nil until it is asked for, which is the one question a generator asks: a
// block that is present and switched off is a project that decided against
// caching, and it should look exactly like a project that never wrote one.
func TestCacheIsNilUntilItIsAskedFor(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"", authOn, authOn + "cache:\n  enabled: false\n"} {
		p, out := parseCache(t, body)
		if out != "" {
			t.Fatalf("this configuration should be valid:\n%s", out)
		}
		if got := p.Config.Cache.IR(); got != nil {
			t.Errorf("cache %q resolved to %+v, want nil", body, got)
		}
	}
}

// The dependency the block used to carry alone. It is still refused, but not
// here: a table's `cache: true` satisfies it just as an `auth:` block does, and
// this package cannot see table configuration. The rule moved to
// compile.checkCacheHasReaders, which is where both halves are visible, and
// TestACacheNobodyReadsIsRefused there is the test that used to live here.
//
// What this asserts is the half that did not move: a block with nothing beside
// it parses, because whether anything reads it is not a question about the
// block's own values.
func TestTheCacheAloneIsNotRefusedHere(t *testing.T) {
	t.Parallel()

	if _, out := parseCache(t, "cache:\n  enabled: true\n"); out != "" {
		t.Errorf("a cache block should parse on its own; the reader check is compile's:\n%s", out)
	}
	if _, out := parseCache(t, authOn+"cache:\n  enabled: true\n"); out != "" {
		t.Errorf("a cache block with auth was refused:\n%s", out)
	}
}

// Everything the generated wiring passes is resolved here, so that what
// server-go writes is readable off one place rather than assembled from a flag
// and four defaults living somewhere else.
func TestCacheDefaultsAreResolved(t *testing.T) {
	t.Parallel()

	p, out := parseCache(t, authOn+"cache:\n  enabled: true\n")
	if out != "" {
		t.Fatalf("enabling the cache and configuring nothing should be valid:\n%s", out)
	}

	got := p.Config.Cache.IR()
	if got == nil {
		t.Fatal("an enabled block resolved to nothing")
	}
	if want := project.DefaultCacheTTL.Seconds(); got.TTLSeconds != want {
		t.Errorf("ttl = %vs, want %vs", got.TTLSeconds, want)
	}
	if got.Channel != project.DefaultCacheChannel {
		t.Errorf("channel = %q, want %q", got.Channel, project.DefaultCacheChannel)
	}
	if got.MaxEntries != project.DefaultCacheMaxEntries {
		t.Errorf("max_entries = %d, want %d", got.MaxEntries, project.DefaultCacheMaxEntries)
	}
}

// What is written is what is carried, so that a deployment separating two
// projects on one database gets the channel it named.
func TestCacheCarriesWhatWasWritten(t *testing.T) {
	t.Parallel()

	p, out := parseCache(t, authOn+"cache:\n  enabled: true\n  ttl: 5s\n"+
		"  channel: rig_cache_demo\n  max_entries: 1000\n")
	if out != "" {
		t.Fatalf("this configuration should be valid:\n%s", out)
	}

	got := p.Config.Cache.IR()
	if got.TTLSeconds != (5 * time.Second).Seconds() {
		t.Errorf("ttl = %vs, want 5s", got.TTLSeconds)
	}
	if got.Channel != "rig_cache_demo" {
		t.Errorf("channel = %q", got.Channel)
	}
	if got.MaxEntries != 1000 {
		t.Errorf("max_entries = %d, want 1000", got.MaxEntries)
	}
}

// The refusals, each of them a number somebody could write and believe in.
func TestTheCacheRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		what string
		body string
	}{
		{"a block nothing reads", authOn + "cache:\n  ttl: 5s\n"},
		{"a lifetime that caches nothing", authOn + "cache:\n  enabled: true\n  ttl: -5s\n"},
		{"a bound that is not one", authOn + "cache:\n  enabled: true\n  max_entries: -1\n"},
		// The name reaches Postgres quoted in a LISTEN and as a parameter to
		// pg_notify, and cache.NewBus panics on one that is not an identifier —
		// so it is refused here, where the diagnostic can name the line.
		{"a channel that is not an identifier", authOn + "cache:\n  enabled: true\n  channel: rig-cache\n"},
		{"a channel starting with a digit", authOn + "cache:\n  enabled: true\n  channel: 1cache\n"},
		{"a channel longer than a name", authOn + "cache:\n  enabled: true\n  channel: " + strings.Repeat("c", 64) + "\n"},
	} {
		if _, out := parseCache(t, c.body); !strings.Contains(out, "RIG3002") {
			t.Errorf("%s was accepted:\n%s", c.what, out)
		}
	}
}

// A warning rather than a refusal, because it is a judgement call: the lifetime
// is only the backstop, and somebody who has decided a long one is acceptable
// for their deployment is not wrong. What they should not be is unaware.
func TestALongLifetimeWarnsRatherThanRefuses(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml",
		[]byte(minimal+authOn+"cache:\n  enabled: true\n  ttl: 2h\n"))
	if diags.HasErrors() {
		t.Fatalf("a long lifetime should be a warning, not an error:\n%s", diags.String())
	}
	if !strings.Contains(diags.String(), "RIG3002") {
		t.Errorf("a two-hour lifetime said nothing at all:\n%s", diags.String())
	}
	if p.Config.Cache.IR() == nil {
		t.Error("a warned-about block still has to resolve")
	}
}
