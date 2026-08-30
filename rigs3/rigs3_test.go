package rigs3

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
)

// The prefix the lifecycle check compares against is a string, and the keys it
// is about come from a function. This is what keeps them in step.
func TestTheKeyPrefixIsStillRight(t *testing.T) {
	t.Parallel()

	for range 100 {
		if key := blob.Key(uuid.New()); !strings.HasPrefix(key, keyPrefix) {
			t.Fatalf("blob.Key produced %q, which does not start with %q — the lifecycle "+
				"check is now comparing rules against a prefix rig does not write", key, keyPrefix)
		}
	}
}

// Rounded up, because the bytes have to outlast the row. Down would accept a
// bucket that deletes them on the last day somebody could still ask for them.
func TestTheWindowIsWholeDaysRoundedUp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		window time.Duration
		want   int64
	}{
		{30 * 24 * time.Hour, 30},
		{7 * 24 * time.Hour, 7},
		{25 * time.Hour, 2},
		{time.Hour, 1},
	}
	for _, c := range cases {
		if got := windowDays(c.window); got != c.want {
			t.Errorf("windowDays(%s) = %d, want %d", c.window, got, c.want)
		}
	}
}

func TestARuleShorterThanTheWindowIsRefused(t *testing.T) {
	t.Parallel()

	window := 30 * 24 * time.Hour

	cases := []struct {
		name  string
		rule  types.LifecycleRule
		short bool
	}{
		{
			name:  "seven days over the whole bucket",
			rule:  rule(7, withFilterPrefix("")),
			short: true,
		},
		{
			name:  "seven days on the prefix rig writes to",
			rule:  rule(7, withFilterPrefix("files/")),
			short: true,
		},
		{
			name:  "seven days on a shard of it",
			rule:  rule(7, withFilterPrefix("files/ab/")),
			short: true,
		},
		{
			// The rule somebody adds on purpose, and the one that costs data.
			name:  "seven days on what rig marked deleted",
			rule:  rule(7, withTag(tagState, string(blob.StateDeleted))),
			short: true,
		},
		{
			name:  "seven days on the pre-Filter spelling",
			rule:  rule(7, withLegacyPrefix("files/")),
			short: true,
		},
		{
			name:  "seven days on somebody else's objects",
			rule:  rule(7, withFilterPrefix("exports/")),
			short: false,
		},
		{
			name:  "ninety days, which outlives the window",
			rule:  rule(90, withFilterPrefix("")),
			short: false,
		},
		{
			name:  "thirty days, which is exactly the window",
			rule:  rule(30, withFilterPrefix("")),
			short: false,
		},
		{
			name:  "seven days, disabled",
			rule:  rule(7, withFilterPrefix(""), disabled),
			short: false,
		},
		{
			name:  "a date rather than a window",
			rule:  expiringOn(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)),
			short: false,
		},
		{
			name:  "a rule that expires nothing",
			rule:  types.LifecycleRule{Status: types.ExpirationStatusEnabled},
			short: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			days, short := tooShort([]types.LifecycleRule{c.rule}, window)
			if short != c.short {
				t.Fatalf("tooShort = (%d, %t), want short=%t", days, short, c.short)
			}
		})
	}
}

// A bucket full of harmless rules and one dangerous one is still a bucket rig
// refuses: the check is about any rule that reaches, not about the first.
func TestOneShortRuleAmongSeveralIsFound(t *testing.T) {
	t.Parallel()

	rules := []types.LifecycleRule{
		rule(365, withFilterPrefix("")),
		rule(7, withFilterPrefix("exports/")),
		rule(9, withFilterPrefix("files/")),
	}
	days, short := tooShort(rules, 30*24*time.Hour)
	if !short || days != 9 {
		t.Fatalf("tooShort = (%d, %t), want (9, true)", days, short)
	}
}

func TestNewRefusesAConfigItCannotUse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
		says string
	}{
		{
			name: "no bucket",
			cfg:  Config{Region: "eu-north-1"},
			says: "no bucket",
		},
		{
			name: "no region and nothing to imply one",
			cfg:  Config{Bucket: "uploads"},
			says: "no region",
		},
		{
			// One environment variable set and not the other, which would
			// otherwise fail somewhere else entirely.
			name: "an access key with no secret",
			cfg:  Config{Bucket: "uploads", Region: "eu-north-1", AccessKeyID: "AKIA"},
			says: "set both",
		},
		{
			name: "a secret with no access key",
			cfg:  Config{Bucket: "uploads", Region: "eu-north-1", SecretAccessKey: "shhh"},
			says: "set both",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// RestoreWindow is zero on purpose: these have to fail before
			// anything reaches for the network.
			_, err := New(context.Background(), c.cfg)
			if err == nil {
				t.Fatal("a store was built from a configuration that names no bucket to write to")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the error does not say what was missing: %v", err)
			}
		})
	}
}

// An endpoint implies a region, because the services behind one mostly have no
// regions and the signer needs a string regardless.
func TestAnEndpointImpliesARegion(t *testing.T) {
	t.Parallel()

	if _, err := New(context.Background(), Config{Bucket: "uploads", Endpoint: "http://127.0.0.1:9000"}); err != nil {
		t.Fatalf("an endpoint should be enough to build a store: %v", err)
	}
}

// Seeking asks the bucket nothing, which is what makes http.ServeContent's
// seek-to-the-end-and-back cost nothing. A nil client is how that is proved:
// anything that reached for one would panic.
func TestSeekingIsArithmetic(t *testing.T) {
	t.Parallel()

	o := &object{key: "files/ab/cd/x", size: 12}

	cases := []struct {
		offset int64
		whence int
		want   int64
	}{
		{0, io.SeekEnd, 12},
		{0, io.SeekStart, 0},
		{4, io.SeekStart, 4},
		{3, io.SeekCurrent, 7},
		{-2, io.SeekEnd, 10},
		{40, io.SeekStart, 40},
	}
	for _, c := range cases {
		got, err := o.Seek(c.offset, c.whence)
		if err != nil {
			t.Fatalf("Seek(%d, %d): %v", c.offset, c.whence, err)
		}
		if got != c.want {
			t.Errorf("Seek(%d, %d) = %d, want %d", c.offset, c.whence, got, c.want)
		}
	}

	if _, err := o.Seek(-1, io.SeekStart); err == nil {
		t.Error("a seek before the start of the object was accepted")
	}
}

// A read past the end is empty rather than the 416 a ranged GET would answer
// with — the behaviour a *bytes.Reader has, and therefore the one blob.Store
// promises. The nil client proves it never asked.
func TestReadingPastTheEndIsEmpty(t *testing.T) {
	t.Parallel()

	o := &object{key: "files/ab/cd/x", size: 5}
	if _, err := o.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	b, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("reading past the end: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("read %d bytes past the end of the object", len(b))
	}
}

// The helpers below build lifecycle rules, because the SDK's own shape is
// pointers all the way down and a table of them would be unreadable.

func rule(days int32, opts ...func(*types.LifecycleRule)) types.LifecycleRule {
	r := types.LifecycleRule{
		Status:     types.ExpirationStatusEnabled,
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(days)},
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func expiringOn(t time.Time) types.LifecycleRule {
	return types.LifecycleRule{
		Status:     types.ExpirationStatusEnabled,
		Expiration: &types.LifecycleExpiration{Date: aws.Time(t)},
		Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("")},
	}
}

func withFilterPrefix(p string) func(*types.LifecycleRule) {
	return func(r *types.LifecycleRule) {
		r.Filter = &types.LifecycleRuleFilter{Prefix: aws.String(p)}
	}
}

func withLegacyPrefix(p string) func(*types.LifecycleRule) {
	//nolint:staticcheck // the spelling a rule written before Filter comes back in
	return func(r *types.LifecycleRule) { r.Prefix = aws.String(p) }
}

func withTag(k, v string) func(*types.LifecycleRule) {
	return func(r *types.LifecycleRule) {
		r.Filter = &types.LifecycleRuleFilter{Tag: &types.Tag{Key: aws.String(k), Value: aws.String(v)}}
	}
}

func disabled(r *types.LifecycleRule) { r.Status = types.ExpirationStatusDisabled }
