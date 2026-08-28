package servergo

import (
	"fmt"
	"time"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// throttleEnabled reports whether this document asked for an API limiter.
func (e *emitter) throttleEnabled() bool {
	return e.doc.API.Throttle != nil && e.doc.API.Throttle.Enabled
}

// throttleWiring emits the constructor Register calls.
func (e *emitter) throttleWiring(b *gobuf.Buf) {
	if !e.throttleEnabled() {
		return
	}

	cfg := e.doc.API.Throttle
	thr := b.Import(runtimeModule + "/throttle")
	dbxPkg := b.Import(runtimeModule + "/dbx")
	ctxPkg := b.Import("context")
	slogPkg := b.Import("log/slog")

	b.Comment("ThrottleLimits are the API rate limits from rig.yaml.\n\n" +
		"They are here rather than inside the limiter so that a test can read the " +
		"numbers the server enforces, and so the generated documentation quotes " +
		"the same ones.")
	b.L("func ThrottleLimits() %s.APILimits {", thr)
	b.L("return %s.APILimits{", thr)
	for _, pair := range []struct {
		field string
		name  string
		limit ir.ThrottleLimit
	}{
		{"ByAPIKey", thr + ".NameAPIKey", cfg.APIKey},
		{"ByAccount", thr + ".NameAccount", cfg.Account},
		{"ByTenant", thr + ".NameTenant", cfg.Tenant},
		{"ByIP", thr + ".NameIP", cfg.IP},
	} {
		if pair.limit.Max <= 0 || pair.limit.WindowSeconds <= 0 {
			continue
		}
		b.L("%s: %s.Limit{Name: %s, Max: %d, Window: %s},",
			pair.field, thr, pair.name, pair.limit.Max, goDuration(b, pair.limit.WindowSeconds))
	}

	if len(cfg.Routes) > 0 {
		b.L("Routes: map[string]%s.Limit{", thr)
		for _, r := range cfg.Routes {
			b.L("%s: {Name: %s.NameRoutePrefix + %s, Max: %d, Window: %s},",
				gobuf.Quote(r.Pattern), thr, gobuf.Quote(r.Pattern), r.Max, goDuration(b, r.WindowSeconds))
		}
		b.L("},")
	}
	if len(cfg.Exempt) > 0 {
		b.L("Exempt: map[string]bool{")
		for _, pattern := range cfg.Exempt {
			b.L("%s: true,", gobuf.Quote(pattern))
		}
		b.L("},")
	}
	b.L("}")
	b.L("}")
	b.NL()

	b.Comment("NewThrottle builds the API limiter over a database.\n\n" +
		"Three layers, and each one is there for a reason worth knowing before " +
		"changing it. Tally is the counter table. Local holds a replica's own " +
		"increments for up to the interval below and only reconciles as a caller " +
		"approaches their limit, which is what keeps this off the hot path — " +
		"without it every API call is a write to a single contended row, and the " +
		"limiter becomes the bottleneck at exactly the traffic it was added for. " +
		"The Limiter on top spends the slots and decides.\n\n" +
		"The cost of the middle layer is stated rather than hidden: the limit is " +
		"approximate across replicas, by at most one interval of traffic each.")
	b.L("func NewThrottle(db %s.Conn, logger *%s.Logger) *%s.Gate {", dbxPkg, slogPkg, thr)
	b.L("if logger == nil { logger = %s.Default() }", slogPkg)
	b.NL()
	b.L("counters := %s.NewLocal(%s.NewTally(db, %s.TallyConfig{}), %s.LocalConfig{",
		thr, thr, thr, thr)
	b.L("Interval: %s,", goDurationFloat(b, cfg.IntervalSeconds))
	b.L("})")
	b.NL()
	b.Comment("Fail open. A counter that cannot answer must not take the API " +
		"down with it — the opposite of what the auth limits do, and deliberately: " +
		"a login limiter that failed open would be a credential-stuffing window " +
		"held open by whoever can make the database slow, while an API limiter " +
		"that failed closed would turn a database blip into the outage it exists " +
		"to prevent.")
	b.L("return %s.NewGate(%s.NewRecording(counters), ThrottleLimits(),", thr, thr)
	b.L("func(ctx %s.Context, err error) {", ctxPkg)
	b.L("logger.WarnContext(ctx, \"rate limit counters unavailable; requests are being served unlimited\", %s.Any(\"error\", err))", slogPkg)
	b.L("})")
	b.L("}")
	b.NL()

	e.throttleSweeper(b)

	e.throttleProxies(b)
}

// throttleSweeper emits the housekeeping task.
//
// The counters are the one runtime table nothing else prunes: the auth log has a
// retention policy, idempotency records have one, and a tally has neither
// because it needs none — a bucket past the longest window cannot be counted by
// anything. What it does need is somebody to delete it, and a table that gains a
// row per caller per window and loses none is not a rate limiter's smallest
// problem for long.
func (e *emitter) throttleSweeper(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		timePkg  = b.Import("time")
		poolPkg  = b.Import("github.com/jackc/pgx/v5/pgxpool")
		servePkg = b.Import(runtimeModule + "/serve")
		thr      = b.Import(runtimeModule + "/throttle")
	)

	dead := 2 * e.throttleLongestWindow()

	b.Comment("ThrottleSweeper deletes rate-limit counters that can no longer be " +
		"counted.\n\n" +
		"Zero takes twice the longest window this project configures, " +
		humanDuration(dead) + ". " +
		"Twice, because a limit reads its own bucket and a weighted slice of the " +
		"one before it, and nothing reads further back. Unlike the auth log there " +
		"is nothing here to get wrong by pruning: deleting a dead bucket cannot " +
		"free a caller who is still over their limit, because nothing was still " +
		"counting it.\n\n" +
		"A task rather than a goroutine, for the reason IdempotencyPruner is one " +
		"— a cron job is one thing running, and a goroutine in every replica is " +
		"as many as there are replicas, all racing to delete the same rows. " +
		"Register it in serve.Config.Tasks and run `<binary> sweep-throttle`:\n\n" +
		"\tTasks: map[string]serve.Task{\"sweep-throttle\": api.ThrottleSweeper(0)},\n\n" +
		"Nothing schedules it for you. Without it rig_throttle keeps a row per " +
		"caller per window for as long as the project runs, and for the " +
		"address limit that is a row per address per window.")
	b.L("func ThrottleSweeper(olderThan %s.Duration) %s.Task {", timePkg, servePkg)
	b.L("return func(ctx %s.Context, pool *%s.Pool) error {", ctxPkg, poolPkg)
	b.L("if olderThan <= 0 { olderThan = %s }", goDuration(b, int64(dead.Seconds())))
	b.L("_, err := %s.NewTally(pool, %s.TallyConfig{}).Sweep(ctx, olderThan, %s.Now())",
		thr, thr, timePkg)
	b.L("return err")
	b.L("}")
	b.L("}")
	b.NL()
}

// throttleLongestWindow is the furthest back any of this project's limits count.
//
// Every window, the route ones included: a route limit is the one most likely to
// be the long one, since a budget worth naming a single route for is often
// hourly or daily.
func (e *emitter) throttleLongestWindow() time.Duration {
	cfg := e.doc.API.Throttle

	longest := time.Minute
	for _, l := range []ir.ThrottleLimit{cfg.APIKey, cfg.Account, cfg.Tenant, cfg.IP} {
		if w := time.Duration(l.WindowSeconds) * time.Second; l.Max > 0 && w > longest {
			longest = w
		}
	}
	for _, r := range cfg.Routes {
		if w := time.Duration(r.WindowSeconds) * time.Second; w > longest {
			longest = w
		}
	}
	return longest
}

// throttleProxies emits the parsed trusted-proxy list.
//
// From `auth.trusted_proxies`, because there is one answer to "where did this
// request come from" and it should not depend on which feature is asking. A
// project with no auth block has no trusted proxies, so the limiter reads the
// peer — which behind a load balancer is the balancer, and is the documented
// reason to set the list.
func (e *emitter) throttleProxies(b *gobuf.Buf) {
	netipPkg := b.Import("net/netip")

	var cidrs []string
	if e.doc.API.Auth != nil {
		cidrs = e.doc.API.Auth.TrustedProxies
	}

	b.Comment("throttleTrustedProxies are the networks whose X-Forwarded-For may " +
		"be believed, from auth.trusted_proxies. Empty trusts none of them.")
	if len(cidrs) == 0 {
		b.L("var throttleTrustedProxies []%s.Prefix", netipPkg)
		b.NL()
		return
	}

	b.Comment("Parsed once at startup rather than per request. The strings were " +
		"validated when rig.yaml was read, so MustParsePrefix cannot panic on a " +
		"document this generator produced.")
	b.L("var throttleTrustedProxies = []%s.Prefix{", netipPkg)
	for _, c := range cidrs {
		b.L("%s.MustParsePrefix(%s),", netipPkg, gobuf.Quote(c))
	}
	b.L("}")
	b.NL()
}

// humanDuration renders a duration the way rig.yaml writes one, for prose. It
// is [goDuration]'s counterpart: the same three cases, and no `0s` tail.
func humanDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

// goDuration renders whole seconds as a duration expression.
func goDuration(b *gobuf.Buf, seconds int64) string {
	timePkg := b.Import("time")
	d := time.Duration(seconds) * time.Second
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%d * %s.Hour", d/time.Hour, timePkg)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d * %s.Minute", d/time.Minute, timePkg)
	default:
		return fmt.Sprintf("%d * %s.Second", d/time.Second, timePkg)
	}
}

// goDurationFloat renders a possibly-sub-second duration.
//
// The interval is the one value here that is reasonably written as `500ms`, so
// unlike every window it cannot go through whole seconds.
func goDurationFloat(b *gobuf.Buf, seconds float64) string {
	timePkg := b.Import("time")
	if seconds <= 0 {
		return "0"
	}
	ms := int64(seconds * 1000)
	if ms%1000 == 0 {
		return fmt.Sprintf("%d * %s.Second", ms/1000, timePkg)
	}
	return fmt.Sprintf("%d * %s.Millisecond", ms, timePkg)
}
