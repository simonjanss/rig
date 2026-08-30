// Package app is the server this example runs: every service, hook and route
// the binary mounts, built by one constructor over a pool.
//
// It is a package rather than a block in main so that the suite in
// integration/ can build exactly what ships — a test cannot import a `main`,
// and a second wiring written for the tests would be a second thing to keep
// true. main.go is what is left once the server moved out: the migrations, the
// three addresses, and the three subcommands that are this application's own.
//
// What it returns is api.Parts, which is generated: one field per thing rig has
// to start, drain or close. api.Main takes it from here.
package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/examples/linearlite/internal/api"
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
	"github.com/simonjanss/rig/runtime/serve"
)

// Config is what this application cannot work out for itself.
//
// A struct rather than an argument each, because the three callers disagree
// about most of it: the server has all of it, dispatch-notifications has a pool
// and a logger, and the integration suite has those plus an App whose ending it
// runs itself. A list of five where three are legitimately nil is a call site
// where nothing names which nil is which.
//
// A pool alone stays enough to construct the whole graph, which is the property
// this exists for.
type Config struct {
	// Pool is the database, and the one field every caller has. Everything below
	// is something some caller legitimately does not.
	Pool *pgxpool.Pool

	// Logger is where this application says things.
	Logger *slog.Logger

	// App is what rig hangs the live subscriptions' drain on — the one ending it
	// has nowhere else to register. Nil from a task, which mounts no routes, and
	// from a test that owns the ending itself.
	App *serve.App

	// Page is the monitoring page when there is a server to mount it on, and nil
	// from a task: a cron entry that dispatched notifications and also served a
	// page over its own five-minute lifetime would be a page nobody could reach.
	Page *observe.Page

	// ElectricURL is the sync service every live subscription forwards to.
	//
	// Empty builds no proxy, and api.Shapes mounts no route without one — which
	// is what a task serving nothing wants, rather than an HTTP client pointed
	// at a development address by a process that will never call it.
	//
	// It is a field because it is a deployment address, the same kind of thing
	// as serve.Config.DatabaseURL and Addr, and main.go is where this program
	// decides those. Read three levels down it would be the one address a reader
	// of main.go could not see.
	ElectricURL string
}

// New is everything this server is made of, as a function taking a [Config] so
// the tasks and the tests can build exactly what ships.
//
// What it builds is identical for all three, which is the reason this is a
// function rather than a block inside the mount closure: the audience for a
// notification is a method on a service, so a job has to be able to build one.
func New(ctx context.Context, cfg Config) (api.Parts, error) {
	// Named once rather than read off cfg thirty times below. The struct is
	// about the call site, where an unlabelled nil is what goes wrong; in here
	// these are what they always were.
	pool, log, page := cfg.Pool, cfg.Logger, cfg.Page

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
		return api.Parts{}, err
	}

	// The proxy every live subscription goes through. It is built before the
	// routes because it is one of the things they are registered with: rig
	// mounts the shapes from api.Handlers.Shapes below, on the same mux and
	// with the same claims lookup and error mapper as everything else.
	//
	// Nothing here is about which rows a subscriber sees. That is the shape's
	// own filter, which rig builds, and the Scope beside it — see the Shapes
	// literal in the Register call.
	//
	// No address, no proxy, and then no shape route: what a caller that serves
	// nothing wants, and what it used to get instead was a connection pool
	// aimed at whatever main.go's default happened to be.
	var proxy *electric.Proxy
	if cfg.ElectricURL != "" {
		proxy, err = electric.New(electric.Config{
			URL: cfg.ElectricURL,

			// Nothing below has a default. Each governs what a subscriber sees
			// while the sync service is away — how long it waits before that counts
			// as an outage, how many rows a snapshot may hand it, when this proxy
			// stops asking — and a value the package chose would be one nobody
			// chose, found the first time a sync service goes away. The Default
			// constants beside them in electric are what these are.
			InitialTimeout:   electric.DefaultInitialTimeout,
			MaxSnapshotRows:  electric.DefaultMaxSnapshotRows,
			SnapshotTimeout:  electric.DefaultSnapshotTimeout,
			BreakerThreshold: electric.DefaultBreakerThreshold,
			BreakerCooldown:  electric.DefaultBreakerCooldown,
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
			return api.Parts{}, err
		}
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

		// The live-sync shapes, on the same mux as everything else. Setting the
		// proxy mounts a stream endpoint per table that asked for one; App is
		// what the drain registers on, so a shutdown lets those subscriptions go
		// rather than spending its budget waiting for one open tab.
		//
		// The todo shapes leave their scope nil, so the generated tenant filter
		// is the whole scope — right for a board the whole tenant shares.
		// Presence is the one that does not, and the reason is in
		// services/rig_presence: every heartbeat is a row change delivered to
		// every subscriber, so its shape is the one place where narrowing is
		// what makes the feature affordable.
		Shapes: api.Shapes{
			App:      cfg.App,
			Proxy:    proxy,
			Presence: rig_presence.Shape,
		},
	})

	// The permission table, made to match what the handlers check — including
	// the auth endpoints' own keys, because minting a personal API key is
	// gated on one of them and this example's settings page does exactly that.
	if err := authz.SyncPermissions(ctx, pool, api.PermissionKeys()); err != nil {
		return api.Parts{}, err
	}

	// The demonstration's own routes: the outbox, what the tour can offer, and
	// the switch that stops the sync service. None is about a table, which is
	// why none is a resource. The proxy goes in because the switch reports what
	// its circuit breaker believes beside what the container is actually doing,
	// and the gap between the two is the thing worth seeing; the URL goes in
	// because a container the kernel gave a port to comes back on a different
	// one, and this is where the process was told which port to forward to.
	registerDemo(mux, mail, page, front.Claims, senders, proxy, cfg.ElectricURL)

	// The front end, same origin as everything above. web/dist is read from
	// disk so `make examples` — which has Go and Docker and deliberately not
	// pnpm — can build and test this server without building the browser half.
	//
	// The `../` is this example's two-half layout showing through: rig.yaml sits
	// above api/ and web/, this binary is built and run from api/, and the other
	// half's output is therefore one directory up. $WEB_DIR is what a deployment
	// sets, where the two halves arrive wherever the image put them.
	mux.Handle("/", spaHandler(cmp.Or(os.Getenv("WEB_DIR"), "../web/dist")))

	// api.Parts rather than a struct of this package's own, and that is the
	// whole reason main.go is four fields and a closure. Every field is
	// something rig starts, drains or closes; naming them in the generated
	// package is what lets the sequence live there too, and what turns
	// forgetting one from a shutdown that misbehaves under load into a build
	// that does not finish.
	// Proxy is the same one Handlers.Shapes above was registered with, named
	// again because the two uses are different questions. There it mounts the
	// routes; here it is asked, once and while refusing to start is still an
	// option, whether the sync service is actually answering — which is the
	// difference between a board that renders and a boot that looked fine.
	return api.Parts{Handler: mux, Engine: engine, Auth: front, Proxy: proxy}, nil
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
	// A pool and a logger, and deliberately nothing else: this is a cron entry.
	// It mounts no routes, so there is no page to reach, no subscription to
	// drain and no sync service to forward one to — and its sends land in its
	// own outbox, which is exactly what a separately deployed dispatcher has.
	parts, err := New(ctx, Config{Pool: pool, Logger: slog.Default()})
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
