package presence_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simonjanss/rig/presence"
)

func TestFresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	ttl := time.Minute

	for _, tc := range []struct {
		name string
		seen time.Time
		want bool
	}{
		{"beat a moment ago", now.Add(-time.Second), true},
		{"beat just inside the window", now.Add(-59 * time.Second), true},
		{"beat exactly at the window", now.Add(-time.Minute), false},
		{"beat before the window", now.Add(-2 * time.Minute), false},
		// The clock a browser passes is its own reading of the freshest row it
		// can see, so it can be behind this row's stamp by a round trip.
		{"a stamp from the future is fresh", now.Add(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &presence.Presence{SeenAt: tc.seen}
			if got := p.Fresh(now, ttl); got != tc.want {
				t.Errorf("Fresh(%s) = %v, want %v", tc.seen, got, tc.want)
			}
		})
	}
}

func TestParseActivity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in      string
		want    presence.Activity
		wantErr bool
	}{
		// Empty is not an error: "present, and not saying more than that" is the
		// commonest thing a client means and should not need a keyword.
		{"", presence.Viewing, false},
		{"viewing", presence.Viewing, false},
		{"editing", presence.Editing, false},
		{"Editing", "", true},
		{"lurking", "", true},
	} {
		got, err := presence.ParseActivity(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseActivity(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseActivity(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseActivity(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNewServiceRefusesAShortTTL is the panic, and it is a test because the
// failure it prevents does not look like a failure: presence that flickers reads
// as a broken feature rather than as a number somebody chose.
func TestNewServiceRefusesAShortTTL(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatalf("a TTL under %s was accepted", presence.MinTTL)
		}
	}()
	presence.NewService(presence.Config{DB: stubDB{}, TTL: time.Second})
}

// TestNewServiceResolvesItsDefaults is the other half: zero means the default,
// not zero.
func TestNewServiceResolvesItsDefaults(t *testing.T) {
	t.Parallel()

	s := presence.NewService(presence.Config{DB: stubDB{}})
	if got := s.TTL(); got != presence.DefaultTTL {
		t.Errorf("TTL = %s, want %s", got, presence.DefaultTTL)
	}
	if got := s.Heartbeat(); got != presence.DefaultHeartbeat {
		t.Errorf("Heartbeat = %s, want %s", got, presence.DefaultHeartbeat)
	}
}

// TestTheDefaultsLeaveRoomForThreeBeats is the relationship the project
// configuration is checked against, asserted on the defaults themselves so the
// two cannot drift.
func TestTheDefaultsLeaveRoomForThreeBeats(t *testing.T) {
	t.Parallel()

	if presence.DefaultTTL < 3*presence.DefaultHeartbeat {
		t.Errorf("the default TTL (%s) is under three default heartbeats (%s): "+
			"one lost request would make somebody vanish",
			presence.DefaultTTL, 3*presence.DefaultHeartbeat)
	}
}

// TestCloseWithoutStartDoesNotWait is the arrangement an operator has who left
// the sweep to the cron job and kept the shutdown registration: there is no
// goroutine, so there is nothing for Close to wait for. Waiting anyway would hold
// shutdown open for the whole registered timeout and report a failure.
func TestCloseWithoutStartDoesNotWait(t *testing.T) {
	t.Parallel()

	sweeper := presence.NewSweeper(presence.SweeperConfig{
		Service: presence.NewService(presence.Config{DB: stubDB{}}),
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sweeper.Close(ctx); err != nil {
		t.Fatalf("Close on a sweeper that was never started: %v", err)
	}
}

// TestCloseHonoursItsContext is the other half, and the reason Close takes one at
// all: a pass that cannot reach the database must not outlive the deadline the
// caller declared for it. notify's engine answers the same way.
func TestCloseHonoursItsContext(t *testing.T) {
	t.Parallel()

	sweeper := presence.NewSweeper(presence.SweeperConfig{
		Service:  presence.NewService(presence.Config{DB: stubDB{}}),
		Interval: time.Hour,
	})
	sweeper.Start()

	// Cancelled before the call, so this is the deadline having already passed
	// rather than a race with the ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A started sweeper stops on its own signal, so either answer is correct
	// here — what is not is blocking forever, which is what this test fails on.
	if err := sweeper.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with a cancelled context: %v", err)
	}
	// And again, on a sweeper that is already closed: idempotent, and not a
	// second close of a channel.
	if err := sweeper.Close(context.Background()); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// TestStartIsIdempotent guards the pair above: two goroutines would both close
// `done` on the way out, which is a panic in a shutdown path.
func TestStartIsIdempotent(t *testing.T) {
	t.Parallel()

	sweeper := presence.NewSweeper(presence.SweeperConfig{
		Service:  presence.NewService(presence.Config{DB: stubDB{}}),
		Interval: time.Hour,
	})
	sweeper.Start()
	sweeper.Start()

	if err := sweeper.Close(context.Background()); err != nil {
		t.Fatalf("Close after two Starts: %v", err)
	}
	// A Start after a Close starts nothing rather than closing `done` twice.
	sweeper.Start()
	if err := sweeper.Close(context.Background()); err != nil {
		t.Fatalf("Close after a Start that followed it: %v", err)
	}
}
