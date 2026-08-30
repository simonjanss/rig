package servergo

import (
	"strconv"
	"strings"
	"time"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// How long each shutdown step rig registers is allowed to take.
//
// They are here rather than as literals at the call sites because they are read
// twice: once by the CloseWithin that registers the step, and once by the budget
// that has to be big enough to hold it. Two copies of a number that must agree
// is the arithmetic this whole file exists to stop a main function doing by
// hand.
const (
	// tracesFlush is a flush to a collector that is not answering. Short,
	// because spans nobody is waiting for must not spend a shutdown.
	tracesFlush = 5 * time.Second
	// notificationsStop is the engine finishing the pass it has claimed. The
	// longest of the three: what is in flight is a write.
	notificationsStop = 15 * time.Second
	// presenceStop is the sweeper's DELETE.
	presenceStop = 5 * time.Second
	// shapesDrain is the live subscriptions being let go. A drain step rather
	// than a close one, and that is the whole point of the number: a poll the
	// server is still holding is a request in flight, so it has to end before
	// the server stops accepting rather than after.
	shapesDrain = 5 * time.Second
	// authClose is the auth cache's invalidation channel. Short, because what
	// it costs is a connection rather than correctness: a listener that has
	// stopped reports itself as not live, and a cache that is not live reads
	// through and holds nothing.
	authClose = 5 * time.Second

	// shutdownHeadroom is what is left for the requests still in flight once
	// rig's own steps have had theirs.
	//
	// serve refuses a budget its parts do not fit inside and warns about one
	// that is exactly spoken for. That warning is the failure this prevents: a
	// shutdown that finishes the housekeeping and drops whatever was being
	// answered is a rolling deploy with dropped requests.
	shutdownHeadroom = 10 * time.Second
)

// The names of the emitted constants, spelled once so the three emitters that
// reference them cannot drift apart.
const (
	tracesConst        = "tracesShutdown"
	notificationsConst = "notificationsShutdown"
	presenceConst      = "presenceShutdown"
	shapesConst        = "shapesShutdown"
	authConst          = "authShutdown"
	headroomConst      = "shutdownHeadroom"
)

// The names each step is registered under, which are the strings serve keys
// [Shutdown] on. Spelled once here for the reason the constant names beside
// them are: the register call and the field that sizes it have to agree, and
// they are written by different emitters.
const (
	tracesStep        = "traces"
	notificationsStep = "notifications"
	presenceStep      = "presence"
	shapesStep        = "shapes"
	authStep          = "auth"
)

// shutdownStep is one of rig's own closers: what it is called in the generated
// source, how long it gets, and what to say about it.
type shutdownStep struct {
	name  string
	took  time.Duration
	about string

	// step is what the closer is registered under, and field is what the
	// emitted Shutdown calls it. Two spellings of one step, because one of them
	// is a string serve matches on and the other is a field a compiler checks.
	step  string
	field string

	// note is what the emitted field says beyond its number, for the step whose
	// name is registered twice. Empty for the four where the field and the
	// closer are one to one.
	note string
}

// shutdownSteps are the closers this project's configuration registers, in the
// order they are declared.
//
// Every one of them is a `CloseWithin` some generated function performs, which
// is why this list and not the mount closure is the honest account of what a
// shutdown costs. A project that registers closers of its own adds to the budget
// rather than replacing it — ShutdownBudget's documentation says how.
func (e *emitter) shutdownSteps() []shutdownStep {
	var steps []shutdownStep
	if e.tracing() {
		steps = append(steps, shutdownStep{tracesConst, tracesFlush, "the trace flush", tracesStep, "Traces", ""})
	}
	if e.hasNotifications() {
		steps = append(steps, shutdownStep{notificationsConst, notificationsStop, "the notification engine", notificationsStep, "Notifications",
			"The engine finishing what it claimed, which is the half of its shutdown " +
				"that has a limit. The half ahead of it — the drain that stops it " +
				"claiming more — is bounded only by what is left of the budget, and " +
				"stays that way whatever this says."})
	}
	if e.hasPresence() {
		steps = append(steps, shutdownStep{presenceConst, presenceStop, "the presence sweeper", presenceStep, "Presence", ""})
	}
	if e.hasShapes() {
		steps = append(steps, shutdownStep{shapesConst, shapesDrain, "the live subscriptions", shapesStep, "Shapes", ""})
	}
	if e.hasAuth() {
		steps = append(steps, shutdownStep{authConst, authClose, "the auth cache's invalidation channel", authStep, "Auth", ""})
	}
	return steps
}

// shutdownBudget is what the emitted ShutdownBudget returns, computed here so
// the doc comment can state the answer as well as the sum.
//
// An operator copying a number into terminationGracePeriodSeconds should not
// have to run the binary to learn it, and a comment that states a total the body
// does not produce is worse than no comment. Both come from this, so they cannot
// disagree.
func (e *emitter) shutdownBudget() time.Duration {
	total := shutdownHeadroom
	for _, s := range e.shutdownSteps() {
		total += s.took
	}
	return total
}

// processFile emits the process around the server: the subcommands this
// project's configuration already decided, the shutdown its own steps add up to,
// and — for a project that traces — the log sink, the provider and the page as
// one object with the order between them settled.
//
// Written for every project, unlike the four files beside it, because every
// project has at least one task to merge into. That is what makes `api.Tasks(…)`
// a line a main function writes once: turning `files:` on later changes what the
// binary can do without changing a line of the main that runs it.
//
// Everything in here that names rig/observe is behind the same predicate
// tracing.gen.go is, so a project that asked for no spans still gets this file
// and still gets no OpenTelemetry anywhere in its dependency graph.
func (e *emitter) processFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.tasksFunc(b)

	e.shutdownConstants(b)
	e.shutdownBudgetFunc(b)
	e.shutdownType(b)

	if e.tracing() {
		e.processType(b)
		e.newProcessFunc(b)
		e.processAccessors(b)
		e.configureMethod(b)
		e.attachMethod(b)
		e.processCloseMethod(b)
	}

	return artifact("process.gen.go", b)
}

// tasksFunc emits the subcommand table.
func (e *emitter) tasksFunc(b *gobuf.Buf) {
	servePkg := b.Import(runtimeModule + "/serve")

	b.Comment("Tasks are the subcommands this project's configuration already " +
		"decided, merged with the ones only this application can write.\n\n" +
		"\tTasks: api.Tasks(map[string]serve.Task{\n" +
		"\t\t\"migrate\": migrate.Apply(migrations, migrate.Options{}),\n" +
		"\t}),\n\n" +
		"Same bargain as [MigrationSources]: the argument is this application's " +
		"half and it wins, so a project that wants a different sweep registers " +
		"one under the same name rather than editing a generated file. What is " +
		"generated is what has no decision left in it — each entry below is one " +
		"call whose every number came from rig.yaml.\n\n" +
		"Nothing schedules any of them. They are subcommands of the binary the " +
		"server is, so that the job and the server it ships with read one " +
		"configuration and connect the same way; what runs them hourly is the " +
		"deployment's business.")
	b.L("func Tasks(own map[string]%s.Task) map[string]%s.Task {", servePkg, servePkg)
	b.L("out := map[string]%s.Task{", servePkg)

	b.Comment("The records of writes that carried an Idempotency-Key. Zero takes " +
		"the default retention, a day.")
	b.L("%s: IdempotencyPruner(0),", gobuf.Quote("prune-idempotency"))

	if e.hasFiles() {
		b.NL()
		b.Comment("Uploads whose row never arrived, and file rows whose restore " +
			"window has closed.")
		e.sweepFilesTask(b)
	}

	if e.hasPresence() {
		b.NL()
		b.Comment("Presence rows past their window, for an operator who would " +
			"rather this were a cron entry than the goroutine " +
			"[StartPresenceSweeper] runs. Unlike a dispatcher it is not the " +
			"guarantee behind anything: who is present is decided by whoever is " +
			"reading, against the clock. This only keeps the table — and every " +
			"new subscriber's first fetch — from carrying yesterday.")
		// Two nil loggers, and both are slog.Default rather than silence: a task
		// is a cron job, and its log is the terminal something started it from.
		// The sweeper's is for a goroutine this form never starts; the task's is
		// the one that writes the report.
		e.poolTask(b, "sweep-presence", "PresenceSweep(NewPresenceSweeper(NewPresence(pool), nil), nil)")
	}

	if e.throttleEnabled() {
		b.NL()
		b.Comment("Counters for windows that have closed. Zero takes the default: " +
			"far enough back that no limit this project states is still counting.")
		b.L("%s: ThrottleSweeper(0),", gobuf.Quote("sweep-throttle"))
	}

	b.L("}")
	b.NL()

	b.Comment("The application's half last, so a name it uses twice is its own " +
		"rather than a silent argument with a generated file.")
	b.L("for name, task := range own {")
	b.L("out[name] = task")
	b.L("}")
	b.L("return out")
	b.L("}")
	b.NL()
}

// poolTask emits one entry whose factory needs the pool the task is handed.
//
// The closure is the whole reason these are generated rather than documented: a
// task is a func(ctx, pool), and every one of these builds its service out of
// the pool it is about to be given. Written by hand it is the same four lines in
// every project, with the pool named three times.
func (e *emitter) poolTask(b *gobuf.Buf, name, call string) {
	var (
		ctxPkg  = b.Import("context")
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
	)

	b.L("%s: func(ctx %s.Context, pool *%s.Pool) error {", gobuf.Quote(name), ctxPkg, poolPkg)
	b.L("return %s(ctx, pool)", call)
	b.L("},")
}

// sweepFilesTask emits the sweeper subcommand, which is the one task whose
// shape depends on where the bytes are kept.
//
// A memory-backed NewFiles cannot fail and nests inside the call. A
// bucket-backed one takes a context and returns an error, because reaching the
// bucket is a thing that can go wrong — so the task builds the service in two
// statements and hands the failure back, which is what makes a cron entry that
// cannot reach the bucket exit non-zero rather than sweep nothing quietly.
func (e *emitter) sweepFilesTask(b *gobuf.Buf) {
	if e.doc.API.Files.Backend != backendS3 {
		e.poolTask(b, "sweep-files", "FileSweeper(NewFiles(pool))")
		return
	}

	var (
		ctxPkg  = b.Import("context")
		poolPkg = b.Import("github.com/jackc/pgx/v5/pgxpool")
	)

	b.L("%s: func(ctx %s.Context, pool *%s.Pool) error {", gobuf.Quote("sweep-files"), ctxPkg, poolPkg)
	b.L("svc, err := NewFiles(ctx, pool)")
	b.L("if err != nil {")
	b.L("return err")
	b.L("}")
	b.L("return FileSweeper(svc)(ctx, pool)")
	b.L("},")
}

// shutdownConstants emits how long each of rig's own closers gets.
func (e *emitter) shutdownConstants(b *gobuf.Buf) {
	timePkg := b.Import("time")

	steps := e.shutdownSteps()

	if len(steps) == 0 {
		// Nothing of rig's to bound, so the whole of the budget is the headroom
		// and there is no block for it to lead.
		b.Comment("What the requests still in flight are worth waiting for.\n\n" +
			"The whole of [ShutdownBudget] here: this project's configuration " +
			"registers no shutdown step of rig's, so there is nothing ahead of the " +
			"requests to bound. A project that closes something of its own adds " +
			"that to the total rather than to this.")
		b.L("const %s = %s", headroomConst, duration(timePkg, shutdownHeadroom))
		b.NL()
		return
	}

	b.Comment("How long each shutdown step rig registers may take.\n\n" +
		"Each has a limit of its own rather than a share of the whole, because a " +
		"step that cannot finish must not be able to spend the budget the steps " +
		"after it will need. They are also what [ShutdownBudget] is made of, " +
		"which is why they are constants rather than literals at the call sites: " +
		"the number a step is registered with and the number the budget counts " +
		"for it are one number.")
	b.L("const (")
	for _, s := range steps {
		b.Comment(strings.ToUpper(s.about[:1]) + s.about[1:] + ".")
		b.L("%s = %s", s.name, duration(timePkg, s.took))
	}
	b.NL()
	b.Comment("What is left for the requests still in flight once the steps above " +
		"have had theirs.\n\n" +
		"serve refuses a budget its parts do not fit inside, and warns about one " +
		"that is exactly spoken for. That warning is the failure this exists to " +
		"prevent: a shutdown that finishes rig's housekeeping and drops whatever " +
		"was being answered is a rolling deploy with dropped requests.")
	b.L("%s = %s", headroomConst, duration(timePkg, shutdownHeadroom))
	b.L(")")
	b.NL()
}

// shutdownBudgetFunc emits the number a main function states as MaxShutdown.
func (e *emitter) shutdownBudgetFunc(b *gobuf.Buf) {
	var (
		timePkg = b.Import("time")
		steps   = e.shutdownSteps()
		total   = e.shutdownBudget()
	)

	parts := make([]string, 0, len(steps)+1)
	terms := make([]string, 0, len(steps)+1)
	if len(steps) == 0 {
		parts = append(parts, "")
	}
	for _, s := range steps {
		parts = append(parts, s.took.String()+" for "+s.about)
		terms = append(terms, s.name)
	}
	if len(steps) > 0 {
		parts = append(parts, shutdownHeadroom.String()+" left over")
	} else {
		parts[0] = shutdownHeadroom.String() + " for the requests still in flight, and no step of rig's ahead of them"
	}
	terms = append(terms, headroomConst)

	b.Comment("ShutdownBudget is what this project's own shutdown needs of " +
		"[github.com/simonjanss/rig/runtime/serve.Config.MaxShutdown].\n\n" +
		"For this project that is " + total.String() + ": " + english(parts) + ".\n\n" +
		"**Read it, then write it down.** There is no default and nothing settles " +
		"it: a serve.Config that leaves MaxShutdown out is refused before the " +
		"server listens, because this is the one number in it that leaves the " +
		"program. So the number is one to look up once and write out:\n\n" +
		"\tMaxShutdown: " + duration("time", total) + "\n\n" +
		"MaxShutdown is the one field in that struct that leaves the program — it " +
		"is what belongs in Kubernetes' terminationGracePeriodSeconds — and " +
		"whoever writes the manifest should be able to read it off the struct " +
		"rather than run the binary or add up a sum spread over three files. " +
		"A project with closers of its own adds theirs to the total above and " +
		"writes that:\n\n" +
		"\tMaxShutdown: " + duration("time", total+5*time.Second) + " // " + total.String() +
		" here, plus 5s for a closer of my own\n\n" +
		"A literal is not a number waiting to drift. serve.App adds up every step " +
		"actually registered, before the server listens, and refuses a budget that " +
		"cannot hold them with the parts named — so a number left stale by a new " +
		"block is a process that will not start and says which parts no longer " +
		"fit. A wrong number that fails loudly at boot is worth more than a right " +
		"one nobody can read.\n\n" +
		"Every step this project's configuration registers is counted here, " +
		"including one in a process that never starts it — a `Tasks:` entry never " +
		"reaches the mount closure, and serve.Config is built before it either " +
		"way. It is a maximum, so counting a step that is not there costs nothing " +
		"but headroom.")
	b.L("func ShutdownBudget() %s.Duration {", timePkg)
	b.L("return %s", strings.Join(terms, " + "))
	b.L("}")
	b.NL()
}

// shutdownType emits the one thing above that a deployment can disagree with.
//
// A struct with a field per step this project registers, rather than a map
// keyed on the names serve matches: the names are strings inside generated code
// and they should stay there, because a misspelled key is a number somebody set
// and nothing read. What a field costs to get wrong is a compilation.
//
// Nothing is emitted for a project with no steps of rig's. There would be no
// fields to put in it, and the headroom is deliberately not one: it is what is
// left for the requests in flight rather than a step, and it is the one number
// here whose being wrong is dropped requests rather than a truncated flush.
func (e *emitter) shutdownType(b *gobuf.Buf) {
	steps := e.shutdownSteps()
	if len(steps) == 0 {
		return
	}

	var (
		cmpPkg   = b.Import("cmp")
		timePkg  = b.Import("time")
		servePkg = b.Import(runtimeModule + "/serve")
	)

	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.field)
	}

	b.Comment("Shutdown is how long each of this project's own shutdown steps may " +
		"take, for a deployment the generated numbers do not suit.\n\n" +
		"\tapi.Main(serve.Config{\n" +
		"\t\t// ...\n" +
		"\t\tShutdown: api.Shutdown{" + steps[0].field + ": " +
		duration("time", steps[0].took/2) + "},\n" +
		"\t}, build)\n\n" +
		"A field left zero keeps what the step was registered with, which is what " +
		"[ShutdownBudget] is the sum of — so the ordinary case is not to write one " +
		"of these at all. It is here because those numbers are rig's answer to what " +
		"a step costs in general, and how long a stop may take is usually decided " +
		"by a terminationGracePeriodSeconds somebody else set. That is a property " +
		"of where this binary runs rather than of what it is, which is why it is a " +
		"field on the serve.Config a main function builds and not a key in " +
		"rig.yaml: the same build runs where the answer is thirty seconds and " +
		"where it is five, and this one can come from the environment.\n\n" +
		func() string {
			if len(names) == 1 {
				return "The only field is " + names[0] + ", because that is the one step "
			}
			return "The fields are " + english(names) + ", and there are no others because " +
				"those are the steps "
		}() +
		"this project's configuration registers. That is the " +
		"whole reason this is generated: a step this server does not have is one " +
		"there is no way to write a number for, and " +
		"[github.com/simonjanss/rig/runtime/serve.Config.Shutdown] takes it as an " +
		"interface so that the type checked here is the one this project has.\n\n" +
		"It does not raise [ShutdownBudget]. serve adds up what was actually " +
		"registered before the server listens and refuses a MaxShutdown they do " +
		"not fit inside, so a number raised here without the total following it is " +
		"a process that will not start and says which part no longer fits. " +
		"[Shutdown.Budget] is that total, for a main function that would rather " +
		"compute it than read it off.")
	b.L("type Shutdown struct {")
	for i, s := range steps {
		if i > 0 {
			b.NL()
		}
		doc := strings.ToUpper(s.about[:1]) + s.about[1:] + ", " +
			duration("time", s.took) + " as generated."
		if s.note != "" {
			doc += "\n\n" + s.note
		}
		b.Comment(doc)
		b.L("%s %s.Duration", s.field, timePkg)
	}
	b.L("}")
	b.NL()

	b.Comment("Steps is how serve reads this, and the reason the fields above are " +
		"the only spelling of a step a main function ever writes.\n\n" +
		"A field left zero is not in what comes back. It has to be left out rather " +
		"than sent as nothing, because a zero step means \"bounded only by what is " +
		"left of the budget\" — so a set that carried its zeroes would turn every " +
		"step it did not mention into an unbounded one.")
	b.L("func (s Shutdown) Steps() []%s.Step {", servePkg)
	b.L("var steps []%s.Step", servePkg)
	for _, s := range steps {
		b.L("if s.%s > 0 {", s.field)
		b.L("steps = append(steps, %s.Step{Name: %s, Timeout: s.%s})",
			servePkg, gobuf.Quote(s.step), s.field)
		b.L("}")
	}
	b.L("return steps")
	b.L("}")
	b.NL()

	b.Comment("Budget is [ShutdownBudget] with this Shutdown's numbers in place of " +
		"the ones it leaves alone:\n\n" +
		"\tshutdown := api.Shutdown{" + steps[0].field + ": " +
		duration("time", steps[0].took/2) + "}\n" +
		"\tapi.Main(serve.Config{\n" +
		"\t\tMaxShutdown: shutdown.Budget(),\n" +
		"\t\tShutdown:    shutdown,\n" +
		"\t}, build)\n\n" +
		"Which is the one place an expression beats the literal [ShutdownBudget]'s " +
		"documentation asks for. That literal is worth writing because an operator " +
		"has to read the number off the struct; a project that has already decided " +
		"to size its own steps has the numbers in front of it either way, and two " +
		"of them to keep in step by hand is one too many.\n\n" +
		"The headroom is in it and cannot be changed. What is left for the requests " +
		"in flight is not a step and is not this struct's to shorten.\n\n" +
		"A DrainDelay is not in it either, for the reason it is not in " +
		"[ShutdownBudget]: it is a number the serve.Config beside this one " +
		"states, and serve counts it against the same total. A project with one " +
		"writes the sum \u2014 which is what [Main] prints when it has a budget " +
		"to complain about.")
	b.L("func (s Shutdown) Budget() %s.Duration {", timePkg)
	b.L("return %s", strings.Join(func() []string {
		terms := make([]string, 0, len(steps)+1)
		for _, s := range steps {
			terms = append(terms, cmpPkg+".Or(s."+s.field+", "+s.name+")")
		}
		return append(terms, headroomConst)
	}(), " + "))
	b.L("}")
	b.NL()
}

// processType emits what a main function holds on to between the two ends of
// the process.
func (e *emitter) processType(b *gobuf.Buf) {
	var (
		obsPkg  = b.Import(observeModule)
		slogPkg = b.Import("log/slog")
	)

	subject := "where the log lines go and where the spans go"
	order := "because the two have an order between them"
	escape := "[Tracing]"
	if e.monitoring() {
		subject = "where the log lines go, where the spans go, and rig's own page over both"
		order = "because the three have an order between them"
		escape = "[Tracing] and [Monitoring]"
	}

	b.Comment("Process is what this application's rig.yaml says about the process " +
		"around its server: " + subject + ".\n\n" +
		"It is generated rather than written because every part of it came from a " +
		"block in rig.yaml, and " + order + " that " +
		"nothing in a main function says out loud. What is left for a main is the " +
		"two ends:\n\n" +
		"\tprocess, err := api.NewProcess()\n" +
		"\tif err != nil {\n" +
		"\t\tslog.Error(\"cannot set this process up\", \"error\", err)\n" +
		"\t\tos.Exit(1)\n" +
		"\t}\n\n" +
		"\tserve.Main(process.Configure(serve.Config{ /* ... */ }),\n" +
		"\t\tfunc(ctx context.Context, app *serve.App) (http.Handler, error) {\n" +
		"\t\t\tprocess.Attach(app)\n" +
		"\t\t\t// ...\n" +
		"\t\t})\n\n" +
		"A project that wants something else — a narrower level on the log file, " +
		"a path that is not an environment variable's to choose — writes those " +
		"lines itself instead. " + escape + " stay" +
		func() string {
			if e.monitoring() {
				return ""
			}
			return "s"
		}() +
		" exported for exactly that: this is the arrangement rig.yaml describes, " +
		"not the only one it allows.")
	b.L("type Process struct {")
	b.L("tracing *%s.Provider", obsPkg)
	b.Comment("How long the flush may take, which is " + tracesConst +
		" unless the serve.Config given to [Process.Configure] said otherwise. " +
		"It is held here because the other half of the flush — [Process.Close], " +
		"which is the half a task run reaches — has no App to have read it from.")
	b.L("traces %s.Duration", b.Import("time"))
	b.L("logs *%s.Logs", obsPkg)
	if e.monitoring() {
		b.L("page *%s.Page", obsPkg)
	}
	b.L("logger *%s.Logger", slogPkg)
	b.L("}")
	b.NL()
}

// newProcessFunc emits the constructor, which is the ordering this whole file
// exists to stop a main function getting wrong.
func (e *emitter) newProcessFunc(b *gobuf.Buf) {
	var (
		ctxPkg  = b.Import("context")
		fmtPkg  = b.Import("fmt")
		obsPkg  = b.Import(observeModule)
		slogPkg = b.Import("log/slog")
	)

	doc := "NewProcess opens the log file and installs the tracer provider"
	if e.monitoring() {
		doc += ", then builds rig's monitoring page over both"
	}
	doc += ".\n\n" +
		"In that order, and the order is the reason this is one call rather than " +
		func() string {
			if e.monitoring() {
				return "three"
			}
			return "two"
		}() +
		". The sink has to exist before the logger that tees into it, " +
		"because the lines written while starting up are lines worth having."
	if e.monitoring() {
		doc += " The provider has to exist before the page, because the page reads " +
			"the span file that provider is writing and two places naming that path " +
			"would be one too many."
	}
	doc += " And all of it has to exist before serve.Config is built, because " +
		"fields of it come out of here and the mount closure runs only once the " +
		"configuration is complete.\n\n" +
		"Nothing here opens a port, talks to a collector or touches the database, " +
		"so it takes no context and there is nothing to bound. Nothing is written " +
		"and nothing exported unless the environment says where — $" +
		observeLogFileEnv + ", $" + observeFileEnv + ", $" + otelEndpointEnv +
		" — and with none of them set this costs one branch per log call and " +
		"still hands out trace identifiers that are real, which is what the " +
		"request id in every error body is.\n\n" +
		"It returns an error rather than exiting, even though the caller has no " +
		"logger to report one with yet: a path nothing can write" +
		func() string {
			if e.monitoring() {
				return ", a monitoring password too short to be worth having"
			}
			return ""
		}() +
		" and a span file that is also the log file are configuration mistakes, " +
		"and which of them it was is worth saying. Deciding a process's exit code " +
		"is not a generator's to do."
	b.Comment(doc)

	b.L("func NewProcess() (*Process, error) {")
	b.L("logs, err := %s.OpenLogs(%s.LogConfig{})", obsPkg, obsPkg)
	b.L("if err != nil {")
	b.L("return nil, %s.Errorf(\"open the log file: %%w\", err)", fmtPkg)
	b.L("}")
	b.NL()

	b.Comment("context.Background rather than a startup budget: setting a provider " +
		"up talks to nothing.")
	b.L("tracing, err := %s.Setup(%s.Background(), Tracing())", obsPkg, ctxPkg)
	b.L("if err != nil {")
	b.L("return nil, %s.Errorf(\"set tracing up: %%w\", err)", fmtPkg)
	b.L("}")
	b.NL()

	if e.monitoring() {
		b.Comment("The one part of the page's configuration that is an object " +
			"rather than a value, which is why it is filled here and not in " +
			"[Monitoring]: the page reading the file this handler is writing is " +
			"what makes a request and the lines it wrote one view, and two places " +
			"naming that path would be one too many.")
		b.L("monitoring := Monitoring()")
		b.L("monitoring.Logs = logs")
		b.NL()
		b.L("page, err := tracing.Page(monitoring)")
		b.L("if err != nil {")
		b.L("return nil, %s.Errorf(\"build the monitoring page: %%w\", err)", fmtPkg)
		b.L("}")
		b.NL()
	}

	b.L("return &Process{")
	b.L("tracing: tracing,")
	b.L("traces: %s,", tracesConst)
	b.L("logs: logs,")
	if e.monitoring() {
		b.L("page: page,")
	}
	b.Comment("Stderr, and the file behind it. The two keep their own levels: " +
		"this one stays at whatever the default handler is set to, and the file " +
		"keeps debug — which is where rig's request line is, so there are " +
		"requests to read back without this process printing one per request to " +
		"a terminal nobody is watching.")
	b.L("logger: %s.New(%s.Tee(%s.Default().Handler(), logs.Handler())),", slogPkg, obsPkg, slogPkg)
	b.L("}, nil")
	b.L("}")
	b.NL()
}

// processAccessors emits the two halves an application may still want to name.
func (e *emitter) processAccessors(b *gobuf.Buf) {
	var (
		obsPkg  = b.Import(observeModule)
		slogPkg = b.Import("log/slog")
	)

	if e.monitoring() {
		b.Comment("Page is rig's monitoring page, for an application with somewhere " +
			"else to name it — a link to it from a page of its own, most likely, " +
			"since the page is on an origin of its own and a relative href does not " +
			"reach it. [github.com/simonjanss/rig/observe.Page.Addr] and BasePath " +
			"are the halves to build that link out of.\n\n" +
			"Serving it is [Process.Configure]'s business rather than this one's.")
		b.L("func (p *Process) Page() *%s.Page { return p.page }", obsPkg)
		b.NL()
	}

	sink := "LogHandler is the log sink"
	if e.monitoring() {
		sink = "LogHandler is the sink the monitoring page reads"
	}
	b.Comment(sink + ", as a handler to tee beside one of your own:\n\n" +
		"\tLogger: slog.New(observe.Tee(myHandler, process.LogHandler()))\n\n" +
		"[Process.Configure] does exactly that with slog.Default's handler, for an " +
		"application with no opinion about it — which is most of them. This is for " +
		"one that has, and it is what keeps setting Logger yourself from being a " +
		"file with nothing in it.")
	b.L("func (p *Process) LogHandler() %s.Handler { return p.logs.Handler() }", slogPkg)
	b.NL()
}

// configureMethod emits the half of a serve.Config that comes out of the
// constructor.
func (e *emitter) configureMethod(b *gobuf.Buf) {
	var (
		obsPkg   = b.Import(observeModule)
		servePkg = b.Import(runtimeModule + "/serve")
	)

	doc := "Configure fills in the half of a serve.Config that comes out of " +
		"[NewProcess], and leaves anything already set alone. What it sets, and " +
		"what setting it yourself costs, is one paragraph each.\n\n"
	if e.monitoring() {
		doc += "Monitor and MonitorAddr are the page, on a listener of its own in " +
			"this same process. Both are zero when the page is unarmed, and then no " +
			"second port is opened at all — which is what a laptop with no password " +
			"set gets. They are filled as a pair or not at all, because either one " +
			"alone is a page somebody believes is running.\n\n"
		doc += "Logger is stderr and the file the page reads, both, each at its own " +
			"level. Set it yourself and the page has nothing to list unless " +
			"[Process.LogHandler] is teed into what you set.\n\n"
	} else {
		doc += "Logger is stderr and the log file, both, each at its own level. " +
			"Set it yourself and the file has nothing in it unless " +
			"[Process.LogHandler] is teed into what you set.\n\n"
	}
	doc += "It does not become slog.Default, and that is a deliberate refusal " +
		"rather than an omission. The handler under this one is the default's " +
		"own — [NewProcess] borrows it so that the format stays whoever's " +
		"main.go set it — and log/slog.SetDefault points the log package's " +
		"output at whatever it is given, so installing this would route that " +
		"output back through a handler that writes to it. That is a process " +
		"which dies at its first line with no log to say why. Hand app.Logger " +
		"to what needs it instead; the constructors here that run something in " +
		"the background take one.\n\n"
	doc += "Pool is a span per statement, from the connection rather than from the " +
		"generated code — so a tracer sees every query, including the ones rig's " +
		"own background work runs and the ones no generator wrote.\n\n" +
		"OnExit is [Process.Close], which is why a main function does not write " +
		"one. serve.Main exits, and `defer` does not survive os.Exit: a flush " +
		"deferred in main runs when the server stopped cleanly and is skipped on " +
		"the three paths where something went wrong — a task that failed, a boot " +
		"that failed, a subcommand that does not exist. Those are the runs whose " +
		"spans somebody actually wants.\n\n" +
		"MaxShutdown is deliberately not one of them, and nothing else fills it " +
		"either. It is the project's to state: the number an operator copies into " +
		"terminationGracePeriodSeconds should be readable off the struct rather " +
		"than out of a call, and a value this method supplied would be one nobody " +
		"wrote deciding how long something outside this process waits before " +
		"SIGKILL. [ShutdownBudget] is what to write."
	b.Comment(doc)

	b.L("func (p *Process) Configure(cfg %s.Config) %s.Config {", servePkg, servePkg)
	if e.monitoring() {
		b.L("if cfg.Monitor == nil && cfg.MonitorAddr == \"\" {")
		b.L("cfg.Monitor = p.page.Handler()")
		b.L("cfg.MonitorAddr = p.page.Addr()")
		b.L("}")
	}
	b.L("if cfg.Logger == nil {")
	b.L("cfg.Logger = p.logger")
	b.L("}")
	b.L("if cfg.Pool == nil {")
	b.L("cfg.Pool = %s.Pool", obsPkg)
	b.L("}")
	b.L("if cfg.OnExit == nil {")
	b.L("cfg.OnExit = p.Close")
	b.L("}")
	b.NL()
	b.Comment("Shutdown is not filled in — it is the deployment's — but it is read " +
		"here, because the flush has two halves and only one of them has an App " +
		"to have been sized through. [Process.Attach] registers the server's " +
		"half on one; [Process.Close] is what a task run reaches, and this is " +
		"where it learns the same number.\n\n" +
		"Read through the interface rather than by asserting [Shutdown] back out " +
		"of it, so that a project filling the field with something of its own is " +
		"sized here too.")
	b.L("if cfg.Shutdown != nil {")
	b.L("for _, s := range cfg.Shutdown.Steps() {")
	b.L("if s.Name == %s && s.Timeout > 0 {", gobuf.Quote(tracesStep))
	b.L("p.traces = s.Timeout")
	b.L("}")
	b.L("}")
	b.L("}")
	b.L("return cfg")
	b.L("}")
	b.NL()
}

// attachMethod emits the mount closure's half.
func (e *emitter) attachMethod(b *gobuf.Buf) {
	servePkg := b.Import(runtimeModule + "/serve")

	b.Comment("Attach registers what this process needs of the server, on the App " +
		"the mount function is given.\n\n" +
		"\tprocess.Attach(app)\n\n" +
		"It is the mount closure's half rather than main's for two reasons that " +
		"are really one: an App is the first thing there is to register a " +
		"shutdown step with, and a logger already writing to the log file is the " +
		"right place to say what is not running.")
	b.L("func (p *Process) Attach(app *%s.App) {", servePkg)

	b.Comment("A limit of its own, because a flush to a collector that is not " +
		"answering must not spend the whole shutdown budget. [Process.Close] is " +
		"the other half of the pair — the half a task run reaches.")
	b.L("app.CloseWithin(%s, p.traces, p.tracing.Shutdown)", gobuf.Quote(tracesStep))
	b.NL()

	if e.monitoring() {
		b.Comment("The database, on the monitoring page, beside whatever else this " +
			"server registers. The probe rather than a state, so the pill answers " +
			"whether the pool can reach Postgres now — which is the question " +
			"somebody looking at that page during an incident is asking, and one " +
			"the request list can only answer for requests that have already " +
			"failed.")
		b.L("p.page.Watch(%s, app.Pool.Ping)", gobuf.Quote("database"))
		b.NL()
	}

	said := "A log file is a deployment's to ask for"
	if e.monitoring() {
		said = "An address and a password are a deployment's to set"
	}
	b.Comment("Said rather than left to be discovered. " + said + ", so a missing " +
		"one is not a failure: it is an environment saying it does not want this. " +
		"Empty for no visible reason is the outcome worth spending a line to avoid.")
	if e.monitoring() {
		b.L("if why := p.page.Unarmed(); why != \"\" {")
		b.L("app.Logger.Info(\"monitoring page not listening\", \"reason\", why)")
		b.L("}")
	}
	b.L("if why := p.logs.Unarmed(); why != \"\" {")
	if e.monitoring() {
		b.L("app.Logger.Info(\"monitoring page has no log lines to read\", \"reason\", why)")
	} else {
		b.L("app.Logger.Info(\"not writing a log file\", \"reason\", why)")
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// processCloseMethod emits the half of the flush a task run reaches.
func (e *emitter) processCloseMethod(b *gobuf.Buf) {
	ctxPkg := b.Import("context")

	b.Comment("Close flushes whatever tracing has buffered, and is the half of that " +
		"a task run reaches.\n\n" +
		"[Process.Configure] sets it as serve.Config.OnExit, so a main function " +
		"does not call it: serve.Main runs it on every way out, including the ones " +
		"that exit non-zero, where a deferred call would not have run at all.\n\n" +
		"Both ways out of this process reach a provider built in [NewProcess], and " +
		"only one of them reaches [Process.Attach]. A `Tasks:` entry never reaches " +
		"the mount closure — serve.Main runs the task and returns — so without " +
		"this an hourly job with $" + observeFileEnv + " set would open a second " +
		"rotating writer on the span file the server is already rotating, and then " +
		"drop everything it buffered on the way out. The server path runs both " +
		"halves and the second finds nothing left to do: Provider.Shutdown is " +
		"idempotent.\n\n" +
		"The log sink is not closed here, and that is not an omission. Its writes " +
		"are unbuffered, so there is nothing to flush, and the last lines a server " +
		"writes are written during its shutdown — a step that closed the file " +
		"would be a step that threw them away.\n\n" +
		"Nothing is returned because there is nowhere left to return it to. A " +
		"failure goes to the logger [Process.Configure] hands the server, which is " +
		"stderr and the log file both.")
	b.L("func (p *Process) Close() {")
	b.L("flushing, cancel := %s.WithTimeout(%s.Background(), p.traces)", ctxPkg, ctxPkg)
	b.L("defer cancel()")
	b.NL()
	b.L("if err := p.tracing.Shutdown(flushing); err != nil {")
	b.L("p.logger.Error(\"cannot flush the spans\", \"error\", err)")
	b.L("}")
	b.L("}")
	b.NL()
}

// duration renders one of this file's own constants as Go source.
//
// The same argument genutil.GoDuration makes, for the durations that are rig's
// rather than the project's: somebody reading the generated file has to be able
// to see that it says five seconds.
func duration(timePkg string, d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10) + " * " + timePkg + ".Second"
}

// english joins a list the way a sentence does.
func english(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
