// Package app is the server this example runs: every service, hook and route
// the binary mounts, built by one constructor over a pool.
//
// It is a package rather than a block in main so that the suite in
// integration/ can build exactly what ships — a test cannot import a `main`,
// and a second wiring written for the tests would be a second thing to keep
// true. main.go is what is left once the server moved out: the log sink, the
// tracing provider, the migrations, and the serve.Config that names them.
package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	genelectric "github.com/simonjanss/rig/examples/linearlite/internal/electric"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/examples/linearlite/services/authz"
	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_presence"
	"github.com/simonjanss/rig/examples/linearlite/services/todo"
	"github.com/simonjanss/rig/examples/linearlite/services/todo_attachment"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/electric"
)

// Parts is what New built, as far as the process around it has to care: the
// routes to serve, and the three things whose lifetime is longer than a
// request's.
//
// A struct rather than four return values because of what the last three are
// for. Each is handed straight to a generated call in main that starts it or
// registers its shutdown, and the day this example grows a fourth, adding a
// field is a line rather than a signature every caller has to be edited for.
type Parts struct {
	// Handler is every route this server answers.
	Handler *http.ServeMux

	// Engine turns a committed notification into inbox lines. Started and
	// drained by api.StartNotificationEngine.
	Engine *notify.Engine

	// Shapes is the live-sync proxy. Drained by api.AttachShapes, which is not
	// optional here in the way the other two might look: a subscription is an
	// in-flight request that nothing else can end.
	Shapes *electric.Proxy

	// Auth holds the invalidation channel for its own caches. Closing it costs a
	// connection rather than correctness, which is exactly why it is easy to
	// forget and worth having a field for.
	Auth *auth.Auth
}

// New is everything this server is made of, as a function taking a pool so
// the tasks and the tests can build exactly what ships.
//
// page is the monitoring page when there is a server to mount it on, and nil
// from a task — a cron entry that dispatched notifications and also served a
// page over its own five-minute lifetime would be a page nobody could reach.
// Everything else is identical either way, which is the reason this is a
// function rather than a block inside the mount closure: the audience for a
// notification is a method on a service, so a job has to be able to build one.
func New(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, page *observe.Page) (*Parts, error) {
	// No Tracer: the generated store.New settles a nil one to observe.Tracer(),
	// which is a package-level value the provider in main installed. A task that
	// never called observe.Setup gets a no-op there and every query below runs
	// untraced rather than differently.
	repos := store.New(pool, store.Config{})

	// The mail this example would have sent — the links the auth package mints,
	// and the email copy of an inbox line. Two interfaces, one ring buffer, and
	// /_demo/outbox shows what went into it. It is per process on purpose:
	// dispatch-notifications gets its own, which is what a separate deployment
	// of the dispatcher actually has.
	mail := outbox.New(20)

	// The inbox's two halves, built in the order the knot demands: the
	// registry first and empty, the engine over it, the service that answers
	// NotifyWho registered into the registry once it exists. The engine can
	// come this early because it does nothing until Start.
	//
	// Two channels, and both record rather than send. rig ships no transport:
	// what it knows is who is owed what and when, and every provider decision
	// after that is one it would get wrong. The inbox itself is not a channel
	// and cannot be turned off — everything here is a copy of a line that was
	// written either way.
	//
	// The two are worth having side by side because they are the two shapes a
	// channel comes in: email has an address on the account and needs nothing
	// registered, and a push has to be told where, which is what a device row
	// is. Mobile is deliberately absent — a channel with no sender has no
	// delivery rows written for it at all, which is the right answer and is
	// what somebody's Mobile preference will show.
	reg := notify.NewRegistry()
	inbox := api.NewNotifications(pool, reg)
	senders := map[notify.Channel]notify.Sender{
		notify.ChannelEmail:   mail.NotificationSender(),
		notify.ChannelDesktop: mail.PushSender(notify.ChannelDesktop),
	}
	engine := api.NewNotificationEngine(pool, reg, senders)

	// The engine's Nudge is handed to the service, so a status change becomes
	// an inbox line moments after its transaction commits rather than at the
	// engine's next tick. An optimization, not a guarantee — the cron task is
	// the guarantee — which is why losing it in a test that passes nil is fine.
	todos := todo.New(repos.Todos, inbox, engine.Nudge, pool)
	attachments := todo_attachment.New(repos.TodoAttachments, api.NewFiles(pool))
	// The tables rig created, and there is no services/ directory for any of
	// them: what may be done to a table rig owns is rig's answer, and the
	// contract is empty because this schema has no rule to add to one. An empty
	// contract is a documented thing to write rather than an oversight — see
	// NewDefaultAccountService — and the alternative was a stub file per table,
	// which is what this used to be: four of them, and three were `nil` in every
	// field.
	//
	// Account is the one this example actually reads. The board needs names to
	// put on assignees, and `auth.expose: [rig_account]` in rig.yaml is what
	// projects it — a resource beside the auth module's own queries against the
	// same table.
	members := api.NewDefaultAccountService(repos.Accounts, api.AccountContract{})

	// The two notification tables a person owns rather than reads: where a push
	// can reach them, and what they want on each channel. `notifications:
	// expose: true` is the whole of it, which is the reason there is no
	// hand-written HTTP for either — and rig's configuration is what makes them
	// owner-scoped, so a read is narrowed to the caller's own rows and a create
	// cannot name somebody else.
	devices := api.NewDefaultNotificationDeviceService(repos.NotificationDevices, api.NotificationDeviceContract{})
	prefs := api.NewDefaultNotificationSettingService(repos.NotificationSettings, api.NotificationSettingContract{})

	reg.Register(api.NewTodoSubject(todos))

	// The authentication foundation, wired from the auth block in rig.yaml.
	// What is left here is the part that is code: who holds a permission, what
	// a new tenant needs, and what happens to a stranger who signs up.
	front, err := api.New(pool, api.Hooks{
		Logger: log,

		// Where a reset link, a confirmation link and an invitation go. Nil
		// would leave those flows unusable rather than silently broken — the
		// token is in the response for a test to read and nothing reaches a
		// person — and this example wants them walked in a browser.
		Notifier: mail,

		// rig derives the permission keys and generates the check; who holds
		// them is this function, over the example's own role tables.
		Grants: authz.Grants(pool),

		// A new tenant gets the three level roles in the transaction that made
		// it, so its Owner can act the moment the response arrives.
		Tenants: account.TenantOptions{
			OnCreated: authz.SeedFor(append(api.PermissionKeys(), authz.AuthKeys()...)),
		},

		// The reason requirement-two of this example works: registering leaves
		// an invitation to the demo tenant waiting in the picker.
		OnRegistered: autoInvite(),
	})
	if err != nil {
		return nil, err
	}

	mux := api.Register(api.Handlers{
		Server: api.Server{
			Auth:   front,
			DB:     pool,
			Logger: log,
		},
		Account:             members,
		Todo:                todos,
		TodoAttachment:      attachments,
		NotificationDevice:  devices,
		NotificationSetting: prefs,
		Notifications:       inbox,
		// Who is here. Setting it mounts the three routes under /presence; nil
		// leaves them unmounted, which is what a project that generated the
		// wiring and has not written the front end yet wants.
		//
		// No service layer, and nothing to register: presence has no rules of
		// this schema's to enforce. The account comes from the credential and
		// the target table is checked against PresenceTargets(), which rig
		// wrote from the compiled document.
		Presence: api.NewPresence(pool),
	})

	// The live-sync shapes, on the same mux as everything else. The proxy is
	// the only thing a browser talks to: it authenticates the subscriber with
	// the same claims lookup the handlers use, builds the tenant filter, and
	// forwards to the sync service `rig db up` started.
	//
	// The todo shapes leave their scope nil, so the generated tenant filter is
	// the whole scope — right for a board the whole tenant shares. Presence is
	// the one that does not, and the reason is in services/rig_presence: every
	// heartbeat is a row change delivered to every subscriber, so its shape is
	// the one place where narrowing is what makes the feature affordable.
	upstream := cmp.Or(os.Getenv("ELECTRIC_URL"), genelectric.DefaultElectricURL)
	proxy, err := electric.New(electric.Config{
		URL: upstream,
		// And what answers a shape when that sync service cannot be reached.
		// One field, and every shape survives an outage on a snapshot of its
		// own rows — the board, the trash, one row's history and the bell, each
		// of which renders nothing without them.
		//
		// It is the same pool everything else here reads from, and it works
		// because a shape is a SELECT: the filter the proxy sent upstream is
		// the filter it runs here, so there is nothing to keep in step and no
		// way for a scope to narrow the stream and not the snapshot. Presence
		// is the one shape that stays a 502, and rig decided that rather than
		// this file — a snapshot of who was here a moment ago that then stops
		// updating is worth less than the empty room it already shows.
		//
		// The cost is in one place: every subscriber falls back at the same
		// moment, so an outage is one read per shape per subscriber against the
		// database the sync service was shielding. MaxSnapshotRows bounds each
		// one and SnapshotTimeout bounds how long it may take.
		DB: pool,
		// Why the sync service was not the one that answered. There is no logger
		// inside the proxy, on purpose, so this is the only way the reason for a
		// 502 on a shape route reaches the log everything else writes to.
		OnError: func(ctx context.Context, err error) {
			log.ErrorContext(ctx, "live sync", slog.Any("error", err))
		},
		// And whether it is there at all, which is twice per outage rather than
		// once per request: the line worth alerting on, where the errors above
		// are one per subscriber and mostly repeat each other.
		OnSyncState: func(ctx context.Context, reachable bool) {
			if reachable {
				log.InfoContext(ctx, "live sync is answering again")
				return
			}
			log.WarnContext(ctx, "live sync is not answering; shapes with a fallback are serving snapshots")
		},
	})
	if err != nil {
		return nil, err
	}
	// One scope, and nothing else. Surviving a sync outage is the proxy's DB
	// field above rather than a line per shape here, which is most of what this
	// registration used to be.
	genelectric.Register(mux, genelectric.Handlers{
		Server:   genelectric.Server{Proxy: proxy, GetClaims: front.Claims},
		Presence: rig_presence.Shape,
	})

	// The permission table, made to match what the handlers check — including
	// the auth endpoints' own keys, because minting a personal API key is
	// gated on one of them and this example's settings page does exactly that.
	if err := authz.SyncPermissions(ctx, pool, api.PermissionKeys()); err != nil {
		return nil, err
	}

	// The demonstration's own routes: the outbox, what the tour can offer, and
	// the switch that stops the sync service. None is about a table, which is
	// why none is a resource. The proxy goes in because the switch reports what
	// its circuit breaker believes beside what the container is actually doing,
	// and the gap between the two is the thing worth seeing; the URL goes in
	// because a container the kernel gave a port to comes back on a different
	// one, and this is where the process was told which port to forward to.
	registerDemo(mux, mail, page, front.Claims, senders, proxy, upstream)

	// The front end, same origin as everything above. web/dist is read from
	// disk so `make examples` — which has Go and Docker and deliberately not
	// pnpm — can build and test this server without building the browser half.
	mux.Handle("/", spaHandler(cmp.Or(os.Getenv("WEB_DIR"), "web/dist")))

	return &Parts{Handler: mux, Engine: engine, Shapes: proxy, Auth: front}, nil
}

// autoInvite is what happens when a stranger signs themselves up: an account
// in the seeded tenant, an invitation waiting in the picker, and the member
// role attached so accepting it lands on a board they can use.
//
// It runs inside the registration transaction — dbx.Tx is how the role grant
// joins it — so a failure here rolls the whole sign-up back rather than
// leaving a person half-invited. A database nobody has seeded is not a
// failure: registration still works, and the newcomer lands in an empty
// picker with "create your own workspace" as the way forward.
func autoInvite() func(context.Context, *account.Service, account.Registered) error {
	tenantID := uuid.MustParse(SeedTenantID)

	return func(ctx context.Context, accounts *account.Service, in account.Registered) error {
		tx, ok := dbx.Tx(ctx)
		if !ok {
			return errors.New("linearlite: OnRegistered expected a transaction")
		}

		var seeded bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM rig_tenant WHERE id = $1 AND deleted_at IS NULL`,
			tenantID).Scan(&seeded); err != nil {
			// pgx answers a missing row as an error; any other failure would
			// resurface on the very next statement, so one branch covers both.
			return nil
		}
		if !seeded {
			return nil
		}

		acct, err := accounts.Provision(ctx, account.ProvisionInput{
			TenantID:     tenantID,
			EmailAddress: in.EmailAddress,
			DisplayName:  in.DisplayName,
			// The invitation is the point: it is what the picker lists, and
			// accepting it is what turns the identity session into a tenant one.
			Invite: true,
		})
		if err != nil {
			return err
		}

		// The role in the same transaction as the account, for the same reason
		// tenant creation seeds roles in its own: an invitation accepted onto a
		// board you cannot read would look exactly like a bug.
		if err := authz.SeedRoles(ctx, tx, tenantID,
			append(api.PermissionKeys(), authz.AuthKeys()...)); err != nil {
			return err
		}
		return authz.AttachRole(ctx, tx, tenantID, acct.ID, string(account.RoleBasic))
	}
}

// DispatchNotifications is the inbox's cron half, built from the same
// constructor the server uses because the audience is a method on a service.
func DispatchNotifications(ctx context.Context, pool *pgxpool.Pool) error {
	// No page: this is a cron entry, and its sends land in its own outbox —
	// which is exactly what a separately deployed dispatcher has.
	parts, err := New(ctx, pool, slog.Default(), nil)
	if err != nil {
		return err
	}
	// The generated task rather than its steps written out again. It resolves,
	// dispatches and prunes; this function used to do only the first, so the mail
	// it was resolving was never actually sent from the cron path.
	return api.NotificationDispatcher(parts.Engine, os.Stdout)(ctx, pool)
}

// accountService builds the account service on its own, for the seed: a
// password set through it is held to the policy in rig.yaml, hashed the same
// way, and recorded in the same trail as one set through the endpoints.
func accountService(pool *pgxpool.Pool) (*account.Service, error) {
	front, err := api.New(pool, api.Hooks{Grants: authz.Grants(pool)})
	if err != nil {
		return nil, err
	}
	return front.Parts().Accounts, nil
}
