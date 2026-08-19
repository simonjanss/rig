package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/tenancy"
)

// Config is everything a [Service] needs that is not a request.
//
// It is resolved from the `notifications:` block in rig.yaml and written into
// the generated wiring, so a claim lease or a retention window is a line in a
// file the documentation can quote rather than a literal in a main function
// nobody diffs.
type Config struct {
	// DB is the pool the subject tables are in. The same one, deliberately: an
	// announcement is written in the transaction that caused it, and it cannot
	// be if the two live in different pools.
	DB DB

	// Registry is how the dispatcher reaches each notifiable table's answers.
	// Nil resolves nothing and says so in every report.
	Registry *Registry

	// Now is the clock, so a test can move it. Nil means time.Now.
	Now func() time.Time
}

func (c Config) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now().UTC()
}

// Service is what a project's own code touches: one method to say that something
// happened, and the inbox reads behind the generated routes.
//
// Everything else — holding an announcement until it is due, asking who should
// hear about it, writing the lines — is [Engine], and no service has to know it
// exists.
type Service struct {
	cfg   Config
	store store
}

// NewService builds the service.
func NewService(cfg Config) *Service {
	return &Service{cfg: cfg, store: store{db: cfg.DB}}
}

// Announce records that something happened.
//
// One line, in a hook the service already has, and it belongs in `After` rather
// than `AfterCommit`: the notification row is part of the change, and a change
// that committed without it is a notification nobody will ever send. That is the
// same argument the hook package makes about the other direction, read backwards.
//
// It carries no recipients and no time. When it is due is the subject's own
// answer, asked here through the registry; who hears about it is asked later, in
// the dispatcher, so that the answer is current rather than however old the
// announcement is.
func (s *Service) Announce(ctx context.Context, a Announcement) (*Notification, error) {
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if a.Subject.Table == "" || a.Subject.ID == uuid.Nil {
		return nil, fmt.Errorf("notify: an announcement needs a subject; use the generated helper")
	}

	n := &Notification{
		ID:        uuid.New(),
		TenantID:  claims.TenantID,
		Kind:      a.Kind,
		State:     StatePending,
		Payload:   a.Payload,
		DeliverAt: s.cfg.now(),
		CreatedAt: s.cfg.now(),

		GroupKey:   a.Group,
		AccountIDs: a.AccountIDs,
	}

	// When it is due is the subject's answer, and a false one means it is not
	// due at all — a post with no publish_at, an event that was cancelled. The
	// row is still written, still linked, and still Cancelled, because a
	// notification that was decided against is a thing that happened.
	if subject := s.cfg.Registry.For(a.Subject.Table); subject != nil {
		at, due, err := subject.DueAt(ctx, a.Subject.ID, a.Kind)
		if err != nil {
			return nil, err
		}
		switch {
		case !due:
			n.State = StateCancelled
		case !at.IsZero():
			n.DeliverAt = at.UTC()
		}
	}

	if err := s.store.insert(ctx, n, a.Subject); err != nil {
		return nil, err
	}
	return n, nil
}

// Reschedule moves every pending notification about a row to a new time, or
// cancels them all.
//
// This is what a `publish_at` that moved reaches, and it is called by the
// generated writer after every update rather than by a hook somebody has to
// remember. Cancellation touches Pending only: mail that is out cannot be
// recalled, and a state transition that pretended otherwise would be a lie the
// schema tells.
func (s *Service) Reschedule(ctx context.Context, subject Subject, at time.Time, due bool) error {
	ids, err := s.pendingAbout(ctx, subject)
	if err != nil || len(ids) == 0 {
		return err
	}
	if !due {
		return s.cancel(ctx, ids)
	}
	if at.IsZero() {
		at = s.cfg.now()
	}
	return s.store.reschedule(ctx, ids, at.UTC())
}
