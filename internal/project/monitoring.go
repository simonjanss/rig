package project

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// The monitoring page's defaults.
//
// Spelled here rather than taken from rig/observe, which is where the page
// lives and declares the same three. `auth:` does the opposite —
// [DefaultAuthBasePath] is auth.DefaultBasePath — and the difference is what
// the import would cost: rig depends on rig/auth already, and depending on
// rig/observe would put OpenTelemetry in the CLI's own binary, for three
// strings. They have to agree, and nothing checks that they do; a rig.yaml
// always resolves these before a generator sees them, so observe's copies are
// reached only by a hand-written main that left the fields zero.
const (
	// DefaultMonitorBasePath is where the page is mounted on its own listener.
	// Nothing else is on that listener to collide with; the prefix is kept
	// because it is the URL projects already have, and because it gives a
	// reverse proxy in front of the port something to key on.
	DefaultMonitorBasePath = "/_rig/monitor"

	// DefaultMonitorMaxTraces is how many requests the page lists.
	DefaultMonitorMaxTraces = 200

	// DefaultMonitorMaxLogs is how many log lines the page reads. Larger than
	// the traces, because one request writes several lines and the request line
	// alone means at least one per request listed.
	DefaultMonitorMaxLogs = 500

	// DefaultMonitorPasswordEnv is the variable the page reads its password
	// from when the project names no other.
	DefaultMonitorPasswordEnv = "RIG_MONITOR_PASSWORD"
)

// Monitoring is rig's own page over what this server wrote about itself: the
// last few hundred requests, what each of them spent its time on, and the log
// lines they produced.
//
// It exists for the deployment too small to be worth a collector and a Grafana
// in front of it, which is most of them for most of their life. It is a reader
// and not a store — the span file `tracing:` already writes is the store — so
// turning it on costs a listener and an HTML asset, and no schema, no retention
// policy and nothing to run beside the server.
//
// Which is also why it cannot be turned on alone: with no spans there is
// nothing to read, and that is refused when the file is read rather than
// discovered as a page that is permanently empty.
type Monitoring struct {
	// Enabled says this project serves the page. Off by default, and off means
	// there is no listener rather than a port answering 404 — `server-go`
	// writes no wiring for a project without it, so the API package names no
	// page at all.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether this project serves rig's monitoring page. It requires tracing.enabled, because the spans are what the page reads."`

	// Addr is where the page listens, as host:port — "127.0.0.1:9090", or
	// ":9090" for every interface. Required when [Monitoring.Enabled] is set.
	//
	// The page has its own listener and its own [net/http.Server] inside the
	// same binary, rather than a route on the API's mux, because the bind
	// address is the only boundary here that a client cannot talk its way
	// around. [Monitoring.Allow] is matched against the connection's own
	// address, which behind a load balancer is the balancer's, so it matches
	// everything or nothing; a port bound to loopback is enforced by the kernel
	// and says the same thing in every deployment.
	//
	// There is no default, deliberately. A default port is one two rig services
	// on a host would fight over, and a default interface is a decision about
	// who can reach a page that lists every path, request id and error cause
	// this server has seen — neither is rig's to make quietly.
	//
	// $RIG_MONITOR_ADDR overrides it at run time, for the reason the span
	// destination is an environment variable: where a thing listens is a
	// property of the deployment, and moving it should not need a regenerate.
	Addr string `yaml:"addr,omitempty" json:"addr,omitempty" jsonschema_description:"Where the monitoring page listens, as host:port, for example 127.0.0.1:9090. Required when monitoring.enabled is set; there is no default. $RIG_MONITOR_ADDR overrides it at run time."`

	// BasePath is where the page is mounted on its own listener, defaulting to
	// [DefaultMonitorBasePath].
	//
	// It has nothing to collide with now that the page is not on the API's mux,
	// so this is a prefix a reverse proxy in front of [Monitoring.Addr] can key
	// on, and the reason the default is kept is that it is the URL projects
	// already have.
	BasePath string `yaml:"base_path,omitempty" json:"base_path,omitempty" jsonschema_description:"Where the page is mounted on its own listener. Defaults to /_rig/monitor."`

	// MaxTraces is how many requests the page lists, newest first. Zero means
	// [DefaultMonitorMaxTraces].
	MaxTraces int `yaml:"max_traces,omitempty" json:"max_traces,omitempty" jsonschema_description:"How many requests the page lists, newest first. Defaults to 200."`

	// MaxLogs is how many log lines the page reads, newest first. Zero means
	// [DefaultMonitorMaxLogs].
	//
	// The lines themselves are a run-time arrangement — a file the deployment
	// names and a handler the application tees in — and this is the one part of
	// them the project decides, for [MaxTraces]'s reason: how much of a page
	// somebody reads is a property of the page.
	MaxLogs int `yaml:"max_logs,omitempty" json:"max_logs,omitempty" jsonschema_description:"How many log lines the page reads, newest first. Defaults to 500."`

	// PasswordEnv names the variable the page reads its password from,
	// defaulting to [DefaultMonitorPasswordEnv]. With nothing in it the page
	// does not listen at all — the port is closed, not guarded — and the server
	// says so once at startup: a project running where there is no password to
	// give is a project that gets no page rather than one that gets an open
	// one.
	PasswordEnv string `yaml:"password_env,omitempty" json:"password_env,omitempty" jsonschema_description:"Variable the page reads its password from. Defaults to RIG_MONITOR_PASSWORD. Empty at run time means the page does not listen at all."`

	// Password is the password itself, for a project that would rather write it
	// here than arrange an environment.
	//
	// It is accepted and it warns, because rig.yaml is checked in and this page
	// shows every path, request id and error cause the server has seen. Whether
	// that trade is worth making is the project's call — a throwaway staging
	// box and a production deployment are not the same decision — and rig makes
	// it once, out loud, rather than for you.
	Password string `yaml:"password,omitempty" json:"password,omitempty" jsonschema_description:"The page's password, written here rather than read from the environment. It warns: rig.yaml is checked in."`

	// Allow is the addresses that may reach the page, as CIDR ranges or single
	// addresses. Empty, the default, allows any and leaves the password as the
	// only check.
	//
	// It narrows rather than replaces: an address that is not on the list is
	// answered 404 before the password is compared, so a scan learns nothing
	// and a leaked password is not enough on its own. There is deliberately no
	// way to have the list without the password — an allowlist keyed on the
	// connection's address is not a boundary behind a load balancer, where
	// every request arrives from the balancer, and that failure is silent and
	// total.
	//
	// [Monitoring.Addr] is the boundary that does hold in that deployment, and
	// this list is the layer above it: the port decides which networks can open
	// a connection at all, and this decides which addresses on those networks
	// are answered.
	//
	// It is matched against the connection's own address and never against a
	// forwarded header, for the reason [Auth.TrustedProxies] exists: an address
	// read from a header a client controls is an address a client chooses.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty" jsonschema_description:"Addresses that may reach the page, as CIDR ranges or single addresses, for example 10.0.0.0/8 or 127.0.0.1. Empty allows any. It narrows the password check rather than replacing it, and is matched against the connection's own address, never a forwarded header."`
}

// Configured reports whether anything in the block was set, so that a block
// somebody filled in and never enabled is refused rather than ignored.
func (m Monitoring) Configured() bool {
	return m.Addr != "" || m.BasePath != "" || m.MaxTraces != 0 || m.MaxLogs != 0 ||
		m.PasswordEnv != "" || m.Password != "" || len(m.Allow) > 0
}

// IR is the resolved block, as a document carries it.
//
// Nil for a project with no page, so that a generator asks the document one
// question rather than reading a flag and deciding what it implies — the same
// shape [Tracing.IR] has, and for the same reason: that nil is what keeps the
// page, and the rig/observe import that serves it, out of a project that never
// asked for either.
func (m Monitoring) IR(serviceName string) *ir.Monitoring {
	if !m.Enabled {
		return nil
	}
	return &ir.Monitoring{
		Enabled:     true,
		ServiceName: serviceName,
		Addr:        m.Addr,
		BasePath:    m.BasePath,
		MaxTraces:   m.MaxTraces,
		MaxLogs:     m.MaxLogs,
		PasswordEnv: m.PasswordEnv,
		Password:    m.Password,
		Allow:       slices.Clone(m.Allow),
	}
}

// applyMonitoringDefaults resolves what the block left out, for the reason
// applyAuthDefaults does: what is written here is what the generated wiring
// passes, and a zero meaning "something downstream decides" would leave two
// places to ask.
func (p *Project) applyMonitoringDefaults() {
	m := &p.Config.Monitoring
	if !m.Enabled {
		return
	}

	setDefault(&m.BasePath, DefaultMonitorBasePath)
	m.BasePath = "/" + strings.Trim(m.BasePath, "/")
	setDefault(&m.PasswordEnv, DefaultMonitorPasswordEnv)
	if m.MaxTraces == 0 {
		m.MaxTraces = DefaultMonitorMaxTraces
	}
	if m.MaxLogs == 0 {
		m.MaxLogs = DefaultMonitorMaxLogs
	}
}

// checkMonitoring refuses a page that cannot work and warns about the one thing
// here that is a judgement call.
func (p *Project) checkMonitoring() diag.List {
	var diags diag.List
	m := p.Config.Monitoring

	if !m.Enabled {
		// The same failure mode the auth and files blocks have: a setting
		// somebody wrote and believed in, silently unread.
		if m.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("monitoring", "enabled"),
				"monitoring is configured but monitoring.enabled is false, so none of it is read; "+
					"set `enabled: true` or remove the block")
		}
		return diags
	}

	// The dependency the tracing milestone said would be a validation rule
	// rather than one shared block. Exporting to a collector somebody else runs
	// and serving a page this server runs are different decisions, and most
	// projects that want the first do not want the second — but the second
	// cannot be had without the first, because the spans are what it reads.
	if !p.Config.Tracing.Enabled {
		diags.Add(diag.CodeMonitoringWithoutTracing, p.At("monitoring", "enabled"),
			"monitoring.enabled needs tracing.enabled: the page reads the spans, and without them it would be empty forever")
	}

	if m.MaxTraces < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("monitoring", "max_traces"),
			"monitoring.max_traces is %d; it has to be positive", m.MaxTraces)
	}
	if m.MaxLogs < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("monitoring", "max_logs"),
			"monitoring.max_logs is %d; it has to be positive", m.MaxLogs)
	}

	// The listener the page gets instead of a route on the API's mux, and the
	// one field here rig will not pick for you. See [Monitoring.Addr] for why
	// there is no default; the short version is that both halves of one are a
	// decision — the port is a thing to collide over and the interface is a
	// decision about who can reach the page.
	if m.Addr == "" {
		diags.Add(diag.CodeMonitoringWithoutAddr, p.At("monitoring", "enabled"),
			"monitoring.enabled needs monitoring.addr: the page listens on its own port, and rig picks no default; "+
				"`addr: 127.0.0.1:9090` serves it to this machine only")
	} else if err := checkListenAddr(m.Addr); err != nil {
		diags.Add(diag.CodeConfigInvalid, p.At("monitoring", "addr"),
			"monitoring.addr is %q: %s; it has to be host:port, for example 127.0.0.1:9090 or :9090", m.Addr, err)
	}

	// Parsed here so that a typo is a diagnostic when rig.yaml is read rather
	// than a server that will not start — on a list where a typo means a page
	// nobody can reach. rig/observe parses it again when the page is built;
	// sharing that would mean this binary importing OpenTelemetry for eight
	// lines over net/netip, which is the same trade the defaults above make.
	for i, entry := range m.Allow {
		if _, err := parseAllowEntry(entry); err != nil {
			diags.Add(diag.CodeConfigInvalid, p.At("monitoring", "allow", strconv.Itoa(i)),
				"monitoring.allow[%d] is %q; it has to be an address or a CIDR range, for example 10.0.0.0/8 or 127.0.0.1", i, entry)
		}
	}

	if m.Password != "" {
		diags.Add(diag.CodeMonitoringPasswordInFile, p.At("monitoring", "password"),
			"monitoring.password is a secret in a file that is checked in; "+
				"leave it out and the page reads $%s instead", m.PasswordEnv)
	}

	return diags
}

// parseAllowEntry accepts one entry of `monitoring.allow`: a CIDR range, or a
// single address standing for itself.
//
// A single address is the common case — one bastion, one office — and making
// somebody write /32 for it is the kind of ceremony that produces a typo.
func parseAllowEntry(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		return netip.ParsePrefix(entry)
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// checkListenAddr refuses an address net.Listen would refuse, so that a typo is
// a diagnostic when rig.yaml is read rather than a server that will not start.
//
// It is deliberately only the shape, and only the port half of that. Whether
// the host resolves and whether the port is free are questions about the
// machine this eventually runs on, and answering them here would mean a `rig
// validate` that passes on a laptop and fails in CI for reasons that have
// nothing to do with the project. The host is left alone for the same reason: a
// literal and a name are both things a project may legitimately mean, and
// telling them apart tells us nothing we would act on.
func checkListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if port == "" {
		return errors.New("no port")
	}
	// "0" is the port that means "any free one", which a test wants and a
	// deployment does not — but refusing it here would be rig deciding what a
	// project may do with its own listener.
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q is not a port number", port)
	}
	return nil
}
