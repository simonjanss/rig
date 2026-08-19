package notify_test

import (
	"testing"
	"time"

	"github.com/simonjanss/rig/notify"
)

func hours(h int) *time.Duration {
	d := time.Duration(h) * time.Hour
	return &d
}

// Stockholm rather than UTC on purpose. Times are read in the account's own
// zone, which is the only reading of a work-hours setting that is not a bug:
// 09:00 means nine where the person is, and a test that ran everything in UTC
// would pass with the zone ignored entirely.
func stockholm(t *testing.T) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	return zone
}

// A delivery outside its window is held, not dropped, and this is the
// arithmetic that decides when it goes out.
func TestAWindowHoldsRatherThanDrops(t *testing.T) {
	t.Parallel()
	zone := stockholm(t)

	// Nine to five, which is the setting people describe.
	s := notify.Setting{
		Channel: notify.ChannelMobile, Enabled: true, Digest: notify.DigestImmediate,
		ActiveFrom: hours(9), ActiveUntil: hours(17), Zone: zone,
	}

	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, time.March, day, hour, minute, 0, 0, zone)
	}

	for _, tc := range []struct {
		name string
		when time.Time
		want time.Time
	}{
		{"inside the window it goes now", at(2, 10, 0), at(2, 10, 0)},
		{"before it opens it waits for the opening", at(2, 7, 30), at(2, 9, 0)},
		// Not "tomorrow at the same time": the next opening is the next day's
		// start of window, which is what somebody means by nine to five.
		{"after it closes it waits for tomorrow's", at(2, 22, 0), at(3, 9, 0)},
		{"the edge is open", at(2, 9, 0), at(2, 9, 0)},
		{"the far edge is closed", at(2, 17, 0), at(3, 9, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := s.NextOpening(tc.when); !got.Equal(tc.want) {
				t.Errorf("next opening from %s = %s, want %s",
					tc.when.Format(time.RFC3339), got.In(zone).Format(time.RFC3339),
					tc.want.Format(time.RFC3339))
			}
		})
	}
}

// A window that wraps midnight is the ordinary way to say "not overnight", and
// it is the case the arithmetic gets wrong.
func TestAWindowThatWrapsMidnight(t *testing.T) {
	t.Parallel()
	zone := stockholm(t)

	// Ten at night until six in the morning. One window, touching two days.
	s := notify.Setting{
		Channel: notify.ChannelDesktop, Enabled: true, Digest: notify.DigestImmediate,
		ActiveFrom: hours(22), ActiveUntil: hours(6), Zone: zone,
	}

	at := func(day, hour int) time.Time {
		return time.Date(2026, time.March, day, hour, 0, 0, 0, zone)
	}

	for _, tc := range []struct {
		name string
		when time.Time
		want time.Time
	}{
		{"late in the evening is inside", at(2, 23), at(2, 23)},
		{"early in the morning is the same window", at(2, 3), at(2, 3)},
		{"the middle of the day waits for the evening", at(2, 12), at(2, 22)},
		{"just after it closes waits for the evening", at(2, 6), at(2, 22)},
		{"just before it opens waits an hour", at(2, 21), at(2, 22)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := s.NextOpening(tc.when); !got.Equal(tc.want) {
				t.Errorf("next opening from %s = %s, want %s",
					tc.when.Format(time.RFC3339), got.In(zone).Format(time.RFC3339),
					tc.want.Format(time.RFC3339))
			}
		})
	}
}

// Weekdays only, and a delivery held on Friday evening lands on Monday morning
// rather than on Saturday.
func TestActiveDaysSkipTheWeekend(t *testing.T) {
	t.Parallel()
	zone := stockholm(t)

	s := notify.Setting{
		Channel: notify.ChannelEmail, Enabled: true, Digest: notify.DigestImmediate,
		ActiveFrom: hours(9), ActiveUntil: hours(17),
		ActiveDays: []int{1, 2, 3, 4, 5}, Zone: zone,
	}

	// 2026-03-06 is a Friday.
	friday := time.Date(2026, time.March, 6, 20, 0, 0, 0, zone)
	monday := time.Date(2026, time.March, 9, 9, 0, 0, 0, zone)

	if got := s.NextOpening(friday); !got.Equal(monday) {
		t.Errorf("held on Friday evening lands at %s, want Monday morning %s",
			got.In(zone).Format(time.RFC3339), monday.Format(time.RFC3339))
	}

	// Saturday itself is not an opening either, however far into it.
	saturday := time.Date(2026, time.March, 7, 11, 0, 0, 0, zone)
	if got := s.NextOpening(saturday); !got.Equal(monday) {
		t.Errorf("Saturday resolves to %s, want %s",
			got.In(zone).Format(time.RFC3339), monday.Format(time.RFC3339))
	}
}

// An unset window is all day, every day, which is what "I said nothing about
// this" has to mean.
func TestAnUnsetWindowIsAlwaysOpen(t *testing.T) {
	t.Parallel()

	s := notify.DefaultSetting(notify.ChannelEmail, notify.DigestImmediate)
	now := time.Date(2026, time.March, 7, 3, 0, 0, 0, time.UTC)

	if got := s.NextOpening(now); !got.Equal(now) {
		t.Errorf("next opening = %s, want the time itself %s", got, now)
	}
}

// The two things called "no mail" are different, and the schema keeps them
// apart because somebody will set the wrong one.
func TestOffIsNotTheSameAsDisabled(t *testing.T) {
	t.Parallel()

	if _, has := notify.DigestOff.Window(); has {
		t.Error("Off has no window: it never sends, rather than waiting for one")
	}
	if _, has := notify.DigestImmediate.Window(); has {
		t.Error("Immediate has no window either: it sends now")
	}

	for digest, want := range map[notify.Digest]time.Duration{
		notify.DigestHourly: time.Hour,
		notify.DigestDaily:  24 * time.Hour,
		notify.DigestWeekly: 7 * 24 * time.Hour,
	} {
		got, has := digest.Window()
		if !has || got != want {
			t.Errorf("%s window = %s (%v), want %s", digest, got, has, want)
		}
	}
}
