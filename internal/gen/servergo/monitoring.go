package servergo

import (
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// observeAddrEnv is observe.AddrEnv, spelled rather than imported for the
// reason internal/project spells the defaults: importing rig/observe would put
// OpenTelemetry in the CLI's binary, and this is one string in one comment.
const observeAddrEnv = "RIG_MONITOR_ADDR"

// monitoring reports whether this document asked for rig's monitoring page.
//
// It cannot be true without [emitter.tracing] — the compiler refuses the
// combination — so everything here may assume the spans it reads exist.
func (e *emitter) monitoring() bool {
	return e.doc.API.Monitoring != nil && e.doc.API.Monitoring.Enabled
}

// monitoringFile emits the one thing a main function needs to serve the page.
//
// A file of its own for the reason tracing.gen.go is one: a project without the
// block gets no file, and so its API package — and its module — names no
// monitoring page at all.
func (e *emitter) monitoringFile() (gen.Artifact, error) {
	m := e.doc.API.Monitoring

	b := gobuf.New(e.cfg.Package)
	obsPkg := b.Import(observeModule)

	b.Comment("Monitoring is this API's monitoring page, as far as generated code " +
		"can know it.\n\n" +
		"The page listens on " + m.Addr + ", on a listener of its own inside this " +
		"same binary. It is not a route on the mux Register returns, and it " +
		"cannot be made into one: which interface the page is bound to is the " +
		"only boundary in front of it that a client cannot talk its way around, " +
		"and sharing the API's listener would be giving that up. Serving it is " +
		"two fields on the server this application already runs:\n\n" +
		"\tpage, err := tracing.Page(api.Monitoring())\n" +
		"\tif err != nil { return nil, err }\n" +
		"\tif why := page.Unarmed(); why != \"\" {\n" +
		"\t\tapp.Logger.Info(\"monitoring page not listening\", \"reason\", why)\n" +
		"\t}\n\n" +
		"\t// ... serve.Config{Monitor: page.Handler(), MonitorAddr: page.Addr()}\n\n" +
		"For the log half, open a sink, tee its handler into the logger this " +
		"application already has, and set it on what this returns:\n\n" +
		"\tlogs, err := observe.OpenLogs(observe.LogConfig{})\n" +
		"\t// ... serve.Config{Logger: slog.New(observe.Tee(base, logs.Handler()))}\n" +
		"\tcfg := api.Monitoring()\n" +
		"\tcfg.Logs = logs\n" +
		"\tpage, err := tracing.Page(cfg)\n\n" +
		"It is not a field a generator can fill in, because it is the sink " +
		"itself rather than a path: the page reading the file the handler is " +
		"writing is what makes a request and the lines it wrote one view, and " +
		"two places naming that path would be one too many. Without it the page " +
		"serves its request half and says why the other is empty.\n\n" +
		"It hangs off the provider rather than standing alone because the page " +
		"reads the span file that provider is writing: two places naming a path " +
		"is two places to get it wrong, and the failure would be a page that is " +
		"permanently empty for no visible reason.\n\n" +
		"What is not here is where the spans go — that is the deployment's, the " +
		"same as it is for [Tracing] — and, unless this project wrote one into " +
		"its rig.yaml, the password, which is read from $" + m.PasswordEnv + " " +
		"at run time. With nothing in it the page does not listen at all: the " +
		"port is closed rather than guarded, and Unarmed is what says so.\n\n" +
		"The address is here and overridable — $" + observeAddrEnv + " wins over " +
		"it — for the reason the span destination is the deployment's: moving a " +
		"port should not need a regenerate. `monitoring.allow` is here because " +
		"which networks may reach the page is a decision about this application " +
		"rather than about the machine it happens to be on. It is the layer " +
		"above the address rather than a substitute for it: the port decides who " +
		"can open a connection, and the list decides which of them is answered, " +
		"404 before the password is compared.")
	b.L("func Monitoring() %s.PageConfig {", obsPkg)
	b.L("return %s.PageConfig{", obsPkg)
	b.L("ServiceName: %s,", gobuf.Quote(m.ServiceName))
	b.L("Addr: %s,", gobuf.Quote(m.Addr))
	b.L("BasePath: %s,", gobuf.Quote(m.BasePath))
	b.L("MaxTraces: %s,", strconv.Itoa(m.MaxTraces))
	b.L("MaxLogs: %s,", strconv.Itoa(m.MaxLogs))
	b.L("PasswordEnv: %s,", gobuf.Quote(m.PasswordEnv))
	if m.Password != "" {
		b.L("Password: %s,", gobuf.Quote(m.Password))
	}
	if len(m.Allow) > 0 {
		quoted := make([]string, 0, len(m.Allow))
		for _, entry := range m.Allow {
			quoted = append(quoted, gobuf.Quote(entry))
		}
		b.L("Allow: []string{%s},", strings.Join(quoted, ", "))
	}
	b.L("}")
	b.L("}")
	b.NL()

	return artifact("monitoring.gen.go", b)
}
