package servergo

import (
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// monitoring reports whether this document asked for rig's monitoring page.
//
// It cannot be true without [emitter.tracing] — the compiler refuses the
// combination — so everything here may assume the spans it reads exist.
func (e *emitter) monitoring() bool {
	return e.doc.API.Monitoring != nil && e.doc.API.Monitoring.Enabled
}

// monitorInterface declares what the page has to be, without naming what it is.
//
// The same trick Authenticator is: a field typed as rig/observe's own would put
// OpenTelemetry in the API package of every project, including the ones with no
// page. One method is the whole surface, so anything that mounts routes fits —
// including a wrapper that puts the page behind something else.
func (e *emitter) monitorInterface(b *gobuf.Buf) {
	if !e.monitoring() {
		return
	}

	b.Comment("MonitoringPage serves rig's page over the spans this server " +
		"wrote.\n\n" +
		"[github.com/simonjanss/rig/observe.Page] satisfies it, and is what " +
		"[Monitoring] is for. Declared here rather than imported so that the type " +
		"of this field is not a dependency.")
	b.L("type MonitoringPage interface {")
	b.Comment("Mount registers the page's routes on the same mux the resource " +
		"routes are on. It registers nothing when there is no password to guard " +
		"it with.")
	b.L("Mount(*%s.ServeMux)", b.Import("net/http"))
	b.L("}")
	b.NL()
}

// monitorField is the page, on the Server struct.
func (e *emitter) monitorField(b *gobuf.Buf) {
	if !e.monitoring() {
		return
	}

	b.Comment("Monitor serves rig's monitoring page, and nil means it is not " +
		"served.\n\n" +
		"\tpage, err := tracing.Page(api.Monitoring())\n\n" +
		"It is mounted on the mux Register returns, after the resource routes. It " +
		"is not itself a traced or logged route — spans and request lines are " +
		"opened inside each generated handler, so nothing that is not one appears " +
		"in either — which is what keeps looking at the page off the page.")
	b.L("Monitor MonitoringPage")
	b.NL()
}

// mountMonitor mounts the page, last.
func (e *emitter) mountMonitor(b *gobuf.Buf) {
	if !e.monitoring() {
		return
	}

	b.NL()
	b.Comment("Last, for the reason the auth routes are late: a collision here " +
		"is a panic naming rig's own page rather than a route this project owns.")
	b.L("if h.Server.Monitor != nil {")
	b.L("h.Server.Monitor.Mount(mux)")
	b.L("}")
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
		"\tpage, err := tracing.Page(api.Monitoring())\n" +
		"\tif err != nil { return nil, err }\n" +
		"\tif why := page.Unarmed(); why != \"\" {\n" +
		"\t\tapp.Logger.Info(\"monitoring page not mounted\", \"reason\", why)\n" +
		"\t}\n\n" +
		"Then set it as Server.Monitor, and it is mounted with the resource " +
		"routes.\n\n" +
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
		"at run time. With nothing in it the page is not mounted at all, which " +
		"is what Unarmed is for.\n\n" +
		"`monitoring.allow` is here, because which networks may reach the page " +
		"is a decision about this application rather than about the machine it " +
		"happens to be on. It narrows the password rather than replacing it: an " +
		"address that is not on the list is answered 404 before the password is " +
		"compared.")
	b.L("func Monitoring() %s.PageConfig {", obsPkg)
	b.L("return %s.PageConfig{", obsPkg)
	b.L("ServiceName: %s,", gobuf.Quote(m.ServiceName))
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
