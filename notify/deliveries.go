package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The three tables delivery adds, and the reads over them.
const (
	DeviceTable   = "rig_notification_device"
	SettingTable  = "rig_notification_setting"
	DeliveryTable = "rig_notification_delivery"
)

// writeDeliveries writes one row per channel a person's settings allow, for one
// inbox line.
//
// It runs inside the transaction that resolved the notification, so the inbox
// line and the copies of it that are owed are one atomic fact. A crash between
// them would leave a line somebody sees in the application and never hears about
// anywhere else — which is survivable and is exactly the kind of "sometimes" a
// notification system is judged on.
//
// A setting that is disabled writes **no row at all** rather than a Skipped one.
// The distinction matters for what a report means: Skipped is a delivery that
// existed and was refused later, and there is nothing to refuse when the person
// said not to make one.
func (e *Engine) writeDeliveries(
	ctx context.Context, n *Notification, recipientID, accountID uuid.UUID, report *Report,
) error {
	if len(e.senders) == 0 {
		return nil
	}

	settings, err := e.settingsFor(ctx, accountID, n.Kind)
	if err != nil {
		return err
	}

	for _, channel := range Channels() {
		if _, registered := e.senders[channel]; !registered {
			// A channel nothing can send on is a channel with no rows. The
			// alternative is a table of Pending copies nobody will ever take.
			continue
		}

		s := settings[channel]
		if !s.Enabled || s.Digest == DigestOff {
			continue
		}

		at := e.cfg.now()
		if close, digested := s.DigestWindowClose(at); digested {
			// The window's close rather than the notification's own time, which
			// is why the due-set query needs no second concept: a digest is a
			// claim whose batch happens to share an account.
			at = close
		}
		// And then held into the window, if the account has one. Held rather
		// than dropped: the inbox line exists either way, and a discarded copy
		// makes the badge and the mailbox disagree.
		if opening := s.NextOpening(at); opening.After(at) {
			at = opening
			report.Held++
		}

		const q = `INSERT INTO ` + DeliveryTable + `
			(id, tenant_id, recipient_id, account_id, channel, kind, digest, deliver_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (recipient_id, channel) DO NOTHING`
		tag, err := e.store.conn(ctx).Exec(ctx, q,
			uuid.New(), n.TenantID, recipientID, accountID, string(channel), n.Kind,
			string(s.Digest), at)
		if err != nil {
			return fmt.Errorf("notify: write delivery: %w", err)
		}
		report.Deliveries += int(tag.RowsAffected())
	}
	return nil
}

// settingsFor resolves one account's answer for every channel, in three steps.
//
// The row for this kind and this channel, else the row for this channel with a
// null kind, else the project default. Two partial unique indexes keep each of
// the first two single, and the ordering below is what puts the more specific
// one last so it wins.
func (e *Engine) settingsFor(ctx context.Context, accountID uuid.UUID, kind string) (map[Channel]Setting, error) {
	out := make(map[Channel]Setting, len(Channels()))
	for _, c := range Channels() {
		out[c] = DefaultSetting(c, e.defaultDigest)
	}

	zone, err := e.zoneOf(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for c, s := range out {
		s.Zone = zone
		out[c] = s
	}

	const q = `SELECT channel, coalesce(kind, ''), is_enabled, digest,
			active_from, active_until, active_days
		FROM ` + SettingTable + `
		WHERE account_id = $1 AND (kind IS NULL OR kind = $2)
		ORDER BY kind NULLS FIRST`

	rows, err := e.store.conn(ctx).Query(ctx, q, accountID, kind)
	if err != nil {
		return nil, fmt.Errorf("notify: read settings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			channel, digest string
			s               Setting
			from, until     *time.Time
			days            []int16
		)
		if err := rows.Scan(&channel, &s.Kind, &s.Enabled, &digest,
			&from, &until, &days); err != nil {
			return nil, fmt.Errorf("notify: scan setting: %w", err)
		}

		s.Channel = Channel(channel)
		s.Digest = Digest(digest)
		s.Zone = zone
		s.ActiveFrom = clockTime(from)
		s.ActiveUntil = clockTime(until)
		for _, d := range days {
			s.ActiveDays = append(s.ActiveDays, int(d))
		}
		out[s.Channel] = s
	}
	return out, rows.Err()
}

// zoneOf is the account's own location, or UTC.
//
// Times are read in it because that is the only reading of a work-hours setting
// that is not a bug: 09:00 means nine where the person is. rig_account.time_zone
// already says "IANA name, for example Europe/Stockholm. Null means UTC", so
// this is reading a decision rather than making one.
func (e *Engine) zoneOf(ctx context.Context, accountID uuid.UUID) (*time.Location, error) {
	var name *string
	err := e.store.conn(ctx).
		QueryRow(ctx, `SELECT time_zone FROM rig_account WHERE id = $1`, accountID).
		Scan(&name)
	if err != nil {
		return time.UTC, nil
	}
	if name == nil || *name == "" {
		return time.UTC, nil
	}
	zone, err := time.LoadLocation(*name)
	if err != nil {
		// A zone the host has no database for is not a reason to stop
		// delivering. UTC and a delivery is better than an error and none.
		return time.UTC, nil
	}
	return zone, nil
}

// clockTime turns a Postgres `time` into how far into a day it is.
func clockTime(t *time.Time) *time.Duration {
	if t == nil {
		return nil
	}
	d := time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second
	return &d
}

// messages groups claimed deliveries into what each channel is handed.
//
// One message per account per channel, which is what makes a digest fall out
// rather than being coded: an Hourly account whose nine copies all came due at
// the same window close is nine deliveries in one message, and an Immediate
// account beside it in the same pass is nine messages of one — because its
// copies were never due at the same moment.
func (e *Engine) messages(ctx context.Context, claimed []Delivery, report *DispatchReport) []Message {
	type key struct {
		account uuid.UUID
		channel Channel
		// alone is what keeps an Immediate row out of somebody else's batch,
		// and out of its own: "tell me as things happen" and "give me a
		// summary" are different requests, and grouping by account alone would
		// answer them the same way.
		alone uuid.UUID
	}

	var (
		order []key
		byKey = make(map[key]*Message, len(claimed))
	)
	for _, d := range claimed {
		k := key{account: d.AccountID, channel: d.Channel}
		if d.Digest == DigestImmediate {
			k.alone = d.ID
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
			byKey[k] = &Message{
				Channel: d.Channel, AccountID: d.AccountID, TenantID: d.TenantID,
			}
		}
		byKey[k].Deliveries = append(byKey[k].Deliveries, d)
	}

	out := make([]Message, 0, len(order))
	for _, k := range order {
		m := byKey[k]
		if len(m.Deliveries) > 1 {
			report.Digested += len(m.Deliveries)
		}
		e.address(ctx, m)
		out = append(out, *m)
	}
	return out
}

// address fills in where a message goes: devices for a push, the account's own
// address for mail.
//
// Errors are swallowed into an empty address on purpose. A message with nowhere
// to go is the channel's problem to report — it knows what it needs — and a
// dispatcher that stopped on one would stop for everybody behind it.
func (e *Engine) address(ctx context.Context, m *Message) {
	if m.Channel == ChannelEmail {
		var address string
		if err := e.store.conn(ctx).QueryRow(ctx,
			`SELECT email_address FROM rig_account WHERE id = $1`, m.AccountID).
			Scan(&address); err == nil {
			m.EmailAddress = address
		}
		return
	}

	rows, err := e.store.conn(ctx).Query(ctx, `
		SELECT id, account_id, channel, token, coalesce(label, ''), last_seen_at
		FROM `+DeviceTable+`
		WHERE account_id = $1 AND channel = $2 AND revoked_at IS NULL`,
		m.AccountID, string(m.Channel))
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d       Device
			channel string
		)
		if err := rows.Scan(&d.ID, &d.AccountID, &channel, &d.Token, &d.Label, &d.LastSeenAt); err != nil {
			return
		}
		d.Channel = Channel(channel)
		m.Devices = append(m.Devices, d)
	}
}
