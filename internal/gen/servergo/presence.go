package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// The two modules a project that tracks presence imports. Neither appears in a
// project that does not, which is what keeps an application with no multiplayer
// surface free of a sweeper it never runs and a browser package it never serves.
const (
	presenceModule     = "github.com/simonjanss/rig/presence"
	presencehttpModule = "github.com/simonjanss/rig/presence/presencehttp"
)

// hasPresence reports whether this project tracks presence at all.
func (e *emitter) hasPresence() bool {
	p := e.doc.API.Presence
	return p != nil && p.Enabled
}

// presenceTargets are the tables a presence may point at, in document order.
//
// Every resource with storage, exposed or not. Not only the exposed ones: a
// browser can be looking at a row of a table that has no REST surface — rig's own
// inbox is exactly that — and refusing its presence would make the check a
// narrower thing than it claims to be.
func (e *emitter) presenceTargets() []string {
	var out []string
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Storage == nil || res.Storage.Table == presenceTable {
			continue
		}
		out = append(out, res.Storage.Table)
	}
	return out
}

// presenceTable is rig's own table, spelled here because this package cannot
// import the compiler.
const presenceTable = "rig_presence"

// presenceFile emits the wiring for a project that tracks presence.
//
// It is written here rather than in the presence module for the reason the inbox
// wiring is: it names the project's own tables. Everything that decides anything
// is in the module; this is the part that could not live there.
func (e *emitter) presenceFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.presenceConstructor(b)
	e.presenceTargetList(b)
	e.presenceSweeper(b)
	e.presenceStarter(b)

	return artifact("presence.gen.go", b)
}

// presenceConstructor emits the service.
func (e *emitter) presenceConstructor(b *gobuf.Buf) {
	var (
		presencePkg = b.Import(presenceModule)
		timePkg     = b.Import("time")
	)
	cfg := e.doc.API.Presence

	b.Comment("NewPresence builds this project's presence service from its " +
		"`presence:` block.\n\n" +
		"Every number here came from that block, so the window a closed laptop " +
		"goes on looking present for is a line in a file the documentation can " +
		"quote rather than a literal in a main function nobody diffs.\n\n" +
		"The pool is the same one the repositories use, and unlike the inbox that " +
		"is a convenience rather than a requirement: nothing here joins a " +
		"caller's transaction. A heartbeat is not part of a change — it is a " +
		"statement about the last twenty seconds — so one that committed beside a " +
		"write that rolled back is still telling the truth.")
	b.L("func NewPresence(db %s.DB) *%s.Service {", presencePkg, presencePkg)
	b.L("return %s.NewService(%s.Config{", presencePkg, presencePkg)
	b.L("DB: db,")
	b.L("TTL: %d * %s.Second,", cfg.TTLSeconds, timePkg)
	b.Comment("Answered to the browser on every beat rather than compiled into " +
		"the front end, so changing it is a deploy of this binary.")
	b.L("Heartbeat: %d * %s.Second,", cfg.HeartbeatSeconds, timePkg)
	b.L("Targets: PresenceTargets(),")
	b.L("})")
	b.L("}")
	b.NL()
}

// presenceTargetList emits the tables a presence may name.
func (e *emitter) presenceTargetList(b *gobuf.Buf) {
	b.Comment("PresenceTargets are the tables a presence may point at.\n\n" +
		"Written from the compiled document, so no table name a request supplies " +
		"is ever taken on trust. It is a typo boundary rather than a security " +
		"one — `target_table` reaches no SQL statement, so there is nothing to " +
		"inject through — but without it the column is untrusted text and every " +
		"reader has to treat it that way.\n\n" +
		"Unexposed resources are in the list. A browser can be looking at a row " +
		"of a table that has no REST surface, and refusing its presence would " +
		"make this a narrower check than it says it is.")
	b.L("func PresenceTargets() []string {")

	targets := e.presenceTargets()
	if len(targets) == 0 {
		b.Comment("This schema has no tables of its own yet, so a presence can " +
			"name a scope and nothing in it. An empty list accepts any table " +
			"name, which is the only thing it can mean: refusing every one would " +
			"make presence unusable rather than safe.")
		b.L("return nil")
		b.L("}")
		b.NL()
		return
	}

	b.L("return []string{")
	for _, table := range targets {
		b.L("%s,", gobuf.Quote(table))
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// presenceSweeper emits the housekeeping, both ways it runs.
func (e *emitter) presenceSweeper(b *gobuf.Buf) {
	var (
		ctxPkg      = b.Import("context")
		presencePkg = b.Import(presenceModule)
		servePkg    = b.Import(runtimeModule + "/serve")
		poolPkg     = b.Import("github.com/jackc/pgx/v5/pgxpool")
		timePkg     = b.Import("time")
	)
	cfg := e.doc.API.Presence

	b.Comment("NewPresenceSweeper builds the housekeeping, for a project that " +
		"wants to start it on terms of its own. [StartPresenceSweeper] is the " +
		"ordinary way — it builds one over the pool the server already has, starts " +
		"it, and registers its shutdown.\n\n" +
		"No Drain step, unlike the notification engine. There is nothing in " +
		"flight worth finishing: a pass interrupted mid-DELETE leaves rows that " +
		"the next pass takes, in whichever replica runs it.\n\n" +
		"A goroutine at all, unlike the dispatcher, and that is a decision rather " +
		"than an inconsistency. The dispatcher is a subcommand because resolving " +
		"an audience twice costs a read and sending twice costs somebody a " +
		"duplicate mail; deleting rows that have already expired is idempotent, " +
		"so two replicas sweeping at once agree and the loser deletes nothing.")
	b.L("func NewPresenceSweeper(svc *%s.Service, logger *%s.Logger) *%s.Sweeper {",
		presencePkg, b.Import("log/slog"), presencePkg)
	b.L("return %s.NewSweeper(%s.SweeperConfig{", presencePkg, presencePkg)
	b.L("Service: svc,")
	b.Comment("app.Logger, so a sweep says what it deleted wherever the server " +
		"says everything else. Nil is not silence — it is slog.Default, which is " +
		"what the cron form below has and all it can have.")
	b.L("Logger: logger,")
	b.L("Interval: %d * %s.Second,", cfg.SweepSeconds, timePkg)
	b.Comment("How long past the TTL a row survives. It is what keeps the two " +
		"expiry mechanisms from disagreeing: a subscriber stops drawing a row at " +
		"the TTL and this deletes it later, so a row is always invisible before " +
		"it is gone rather than the other way round.")
	b.L("Grace: %d * %s.Second,", cfg.GraceSeconds, timePkg)
	b.L("})")
	b.L("}")
	b.NL()

	b.Comment("PresenceSweep is the sweep as a cron job, for an operator who " +
		"would rather it were not a goroutine.\n\n" +
		"**It is not the guarantee behind presence**, and the contrast with " +
		"NotificationDispatcher is the thing to read. There the task is the " +
		"guarantee, because a notification that is never dispatched is lost. " +
		"Here who is present is decided by whoever is reading, against the clock, " +
		"and correctly within a second — this only keeps the table, and every new " +
		"subscriber's first fetch, from carrying yesterday. Skipping it costs " +
		"space.\n\n" +
		"Register it in serve.Config.Tasks and run `<binary> sweep-presence`.\n\n" +
		"The logger is where the pass's report goes, and it is written here " +
		"rather than inside Sweep because the goroutine calls Sweep too and " +
		"that one is a line per interval forever. A nil logger is not silence: " +
		"it is slog.Default, which for a cron job is the terminal it was " +
		"started from. At info, because slog.Default drops debug and a sweep " +
		"nobody can see is one nobody can tell from a cron entry that never " +
		"fired.")
	b.L("func PresenceSweep(sweeper *%s.Sweeper, logger *%s.Logger) %s.Task {",
		presencePkg, b.Import("log/slog"), servePkg)
	b.L("if logger == nil { logger = %s.Default() }", b.Import("log/slog"))
	b.L("return func(ctx %s.Context, _ *%s.Pool) error {", ctxPkg, poolPkg)
	b.L("report, err := sweeper.Sweep(ctx)")
	b.L(`logger.InfoContext(ctx, "presence swept", "counts", report.String())`)
	b.L("return err")
	b.L("}")
	b.L("}")
	b.NL()
}

// presenceStarter emits the three lines every main that tracks presence used to
// copy out of the comment above.
func (e *emitter) presenceStarter(b *gobuf.Buf) {
	servePkg := b.Import(runtimeModule + "/serve")

	b.Comment("StartPresenceSweeper starts the housekeeping and registers its " +
		"shutdown, which is the whole of what a main function does with it:\n\n" +
		"\tapi.StartPresenceSweeper(app)\n\n" +
		"The service it sweeps through is its own, built over app.Pool. Nothing " +
		"in a presence service is stateful — a heartbeat is a write and a read is " +
		"a query — so a second one beside the one the handlers were given is two " +
		"structs over one pool, and the alternative was threading a service out " +
		"of wherever this application happened to build it.\n\n" +
		"A goroutine at all, unlike a dispatcher, and that is a decision rather " +
		"than an inconsistency. Resolving an audience twice costs a read and " +
		"sending twice costs somebody a duplicate mail, so that takes a lease; " +
		"deleting rows that have already expired is idempotent, so two replicas " +
		"sweeping at once agree and the loser deletes nothing. Running this and " +
		"the `sweep-presence` task both is not a mistake either, for the same " +
		"reason.\n\n" +
		"No Drain, because there is nothing in flight worth finishing: a pass " +
		"interrupted mid-DELETE leaves rows that the next pass takes.")
	b.L("func StartPresenceSweeper(app *%s.App) {", servePkg)
	b.L("sweeper := NewPresenceSweeper(NewPresence(app.Pool), app.Logger)")
	b.L("sweeper.Start()")
	b.L("app.CloseWithin(%s, %s, sweeper.Close)", gobuf.Quote(presenceStep), presenceConst)
	b.L("}")
	b.NL()
}
