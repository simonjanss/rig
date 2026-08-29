package notify

import (
	"time"
)

// Setting is one account's answer about one channel.
//
// Resolution is three steps and this package states them once, because a
// settings system whose precedence is folklore is one nobody trusts: **the row
// for this kind and this channel, else the row for this channel with a null
// kind, else the default in rig.yaml.** Two partial unique indexes keep each of
// the first two steps single.
type Setting struct {
	Channel Channel
	// Kind is empty for the row that answers for every kind, which is the second
	// step of the resolution.
	Kind string

	// Enabled false writes no delivery row at all, rather than a Skipped one.
	// See [DigestOff] for how the two differ.
	Enabled bool
	Digest  Digest

	// ActiveFrom and ActiveUntil bound the window, in the account's own zone.
	// Both nil means all day.
	//
	// The window is stated as when to deliver rather than when to stay quiet,
	// and that is not a naming preference. "Mobile, weekdays, nine to five" is
	// one row; as quiet hours it is two, because the quiet period wraps a
	// weekend and a night, and somebody changing the end of their working day
	// would have to reason about the complement.
	ActiveFrom  *time.Duration
	ActiveUntil *time.Duration

	// ActiveDays are ISO weekdays, 1 being Monday. Empty means every day.
	//
	// An array rather than a bitmask, because it appears in a settings payload a
	// client renders and [1,2,3,4,5] is legible in a way 62 is not.
	ActiveDays []int

	// Zone is the account's own, from rig_account.time_zone. Nil means UTC,
	// which is what that column's own documentation says a null means.
	Zone *time.Location
}

// DefaultSetting is what an account with no row at all gets: enabled, on the
// project's default digest, all day, every day.
func DefaultSetting(channel Channel, digest Digest) Setting {
	return Setting{Channel: channel, Enabled: true, Digest: digest}
}

// zone is the account's location, or UTC.
func (s Setting) zone() *time.Location {
	if s.Zone == nil {
		return time.UTC
	}
	return s.Zone
}

// NextOpening is when a delivery due at t may actually go out.
//
// t itself when the window is open, and the next opening when it is not.
// **Outside its window a delivery is held, not dropped**, and that is
// structural rather than a preference: the inbox line exists either way, so a
// channel that silently discarded its copy has made the badge and the mailbox
// disagree, and the person will eventually see the notification and wonder why
// they were never told. Late is a delay; dropped is a lie.
//
// The times are read in the account's own zone, which is the only reading of a
// work-hours setting that is not a bug: 09:00 means nine where the person is.
//
// A window that wraps midnight has to work — 22:00 to 06:00 is the ordinary way
// to say "not overnight" — and it is the arithmetic that is easy to get wrong,
// which is why the wrap is one branch here rather than an assumption spread
// over three.
func (s Setting) NextOpening(t time.Time) time.Time {
	local := t.In(s.zone())

	// Up to a week ahead: seven days covers every combination of a weekday list
	// and a window, and there is no eighth answer.
	for day := range 8 {
		candidate := local
		if day > 0 {
			// Each subsequent day starts at its own opening, so a delivery held
			// on Friday evening lands at Monday's opening rather than at
			// Monday's current time.
			candidate = startOfDay(local.AddDate(0, 0, day), s.zone())
		}
		if !s.dayAllowed(candidate) {
			continue
		}
		if at, ok := s.openingOn(candidate); ok {
			return at.UTC()
		}
	}

	// A window nothing can satisfy — no allowed days at all. Returning t rather
	// than never is the safe direction: a delivery that goes out at an awkward
	// time is a nuisance, and one that never goes out is the thing the whole
	// hold-rather-than-drop rule exists to prevent.
	return t
}

// dayAllowed reports whether this weekday is one the account asked for.
func (s Setting) dayAllowed(t time.Time) bool {
	if len(s.ActiveDays) == 0 {
		return true
	}
	iso := int(t.Weekday())
	if iso == 0 {
		iso = 7 // Go counts Sunday as zero; ISO counts it as seven.
	}
	for _, d := range s.ActiveDays {
		if d == iso {
			return true
		}
	}
	return false
}

// openingOn answers when, on this day, the window is open — or false when it is
// not open again on this day at all.
func (s Setting) openingOn(t time.Time) (time.Time, bool) {
	if s.ActiveFrom == nil || s.ActiveUntil == nil {
		return t, true
	}

	var (
		from  = *s.ActiveFrom
		until = *s.ActiveUntil
		since = sinceMidnight(t)
		start = startOfDay(t, s.zone())
	)

	// The wrapping case: 22:00 to 06:00 is open late in the evening and again
	// early in the morning, and it is one window rather than two even though it
	// touches two days.
	if from > until {
		if since >= from || since < until {
			return t, true
		}
		return start.Add(from), true
	}

	switch {
	case since >= from && since < until:
		return t, true
	case since < from:
		return start.Add(from), true
	default:
		// Past the close, so not again today.
		return time.Time{}, false
	}
}

// DigestWindowClose is when a digest's batch goes out.
//
// The window's close rather than the notification's own time, and that is why
// the due-set query needs no second concept: a digest is a claim whose batch
// happens to share an account, and it becomes due like everything else.
func (s Setting) DigestWindowClose(t time.Time) (time.Time, bool) {
	window, ok := s.Digest.Window()
	if !ok {
		return t, false
	}
	return t.Add(window), true
}

// sinceMidnight is how far into its own day an instant is, in its own zone.
func sinceMidnight(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second
}

func startOfDay(t time.Time, zone *time.Location) time.Time {
	local := t.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
}
