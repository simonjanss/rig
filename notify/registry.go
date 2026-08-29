package notify

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Subjects is what the dispatcher asks about a notifiable table: when
// notifications about a row are due, and who should hear about one.
//
// It is the answer to the reach problem this milestone actually has. NotifyWho
// is a method on a service; the dispatcher is a background job; and today those
// two cannot see each other, because services are built inside the mount
// function and a task is a subcommand handed a pool. The answer is the one the
// delete propagation already uses — the job does not need the service, it needs
// the closure, and the closure carries the service it closed over.
//
// When a notification is due is deliberately not here. That question is asked
// where the row is in hand — in the hook that announces, and in the one that
// reacts to an update — and asking it here would mean reading the row back,
// which a hook inside the write's own transaction cannot do: a generated read
// goes to the pool, so it would not see the row that has not committed yet.
//
// A generator implements this per notifiable table and registers it where the
// service is already wired, so adding a link table and forgetting to register
// does not compile.
type Subjects interface {
	// Table is the subject's own table, which is how the dispatcher finds the
	// right entry for a notification's link row.
	Table() string

	// Audience answers, at the moment of sending, which accounts should hear
	// about a row.
	//
	// It runs under System claims for the row's own tenant, which is what makes
	// the answer current: an account added to the group after the notification
	// was written is in this list, because this list is built now.
	//
	// It must be a pure read, and it may be called more than once for the same
	// notification. The unique index on (notification_id, account_id) makes a
	// repeat harmless; a method with side effects would make it visible.
	Audience(ctx context.Context, n *Notification, subjectID uuid.UUID) ([]uuid.UUID, error)
}

// Registry is every notifiable table's answers, by table name.
//
// Built once where the services are, and handed to the engine and to the task
// alike — which is the whole reason a project's main.go grows a constructor both
// call instead of building services inside the mount closure.
type Registry struct {
	byTable map[string]Subjects
}

// NewRegistry collects the subjects a project has.
//
// Registering the same table twice is a panic rather than a silent overwrite: it
// means two services believe they own one table's audience, and whichever one
// lost would go on looking wired while nobody it was supposed to notify heard
// anything.
func NewRegistry(subjects ...Subjects) *Registry {
	r := &Registry{byTable: make(map[string]Subjects, len(subjects))}
	for _, s := range subjects {
		if s == nil {
			continue
		}
		if _, dup := r.byTable[s.Table()]; dup {
			panic(fmt.Sprintf("notify.NewRegistry: %s is registered twice", s.Table()))
		}
		r.byTable[s.Table()] = s
	}
	return r
}

// Register adds a subject after the registry exists.
//
// It is here because of the order services have to be built in: a service needs
// the notify service to announce anything, and the notify service needs the
// registry to ask when a notification is due — so the registry is made first,
// empty, and filled once the services it points at exist. That is a knot rather
// than a design, and this is the one place it shows.
//
// Registering the same table twice is a panic rather than a silent overwrite,
// for the reason [NewRegistry] gives.
func (r *Registry) Register(subjects ...Subjects) {
	if r.byTable == nil {
		r.byTable = make(map[string]Subjects, len(subjects))
	}
	for _, s := range subjects {
		if s == nil {
			continue
		}
		if _, dup := r.byTable[s.Table()]; dup {
			panic(fmt.Sprintf("notify.Registry.Register: %s is registered twice", s.Table()))
		}
		r.byTable[s.Table()] = s
	}
}

// For returns the subject registered for a table, or nil.
//
// Nil is not an error here and is worth saying why: a project can have
// notifications whose subject table was dropped from a later build, and a
// dispatcher that failed on one would stop dispatching everything behind it. The
// engine reports them instead, in the count that exists for exactly this.
func (r *Registry) For(table string) Subjects {
	if r == nil {
		return nil
	}
	return r.byTable[table]
}

// Tables are the registered tables, for a startup log line that says what this
// process can resolve.
func (r *Registry) Tables() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byTable))
	for table := range r.byTable {
		out = append(out, table)
	}
	return out
}
