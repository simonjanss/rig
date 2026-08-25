package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// monitored is the traced fixture with the page on as well. It is never the
// other way round: the compiler refuses monitoring without tracing, because the
// page is a reader over the span file and has nothing to read without one.
func monitored(t *testing.T) *ir.Document {
	t.Helper()

	doc := traced(t)
	doc.API.Monitoring = &ir.Monitoring{
		Enabled:     true,
		ServiceName: "lifecycle",
		Addr:        "127.0.0.1:9090",
		BasePath:    "/_rig/monitor",
		MaxTraces:   200,
		MaxLogs:     500,
		PasswordEnv: "RIG_MONITOR_PASSWORD",
	}
	return doc
}

func TestMonitoringGolden(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, servergo.New(), monitored(t), opts())
	gentest.Golden(t, filepath.Join("testdata", "monitoring"), artifacts, *update)
}

// Absent rather than disabled. A project that traces and did not ask for the
// page gets no page: no file, and nothing anywhere else that names one.
func TestWithoutTheBlockThereIsNoPage(t *testing.T) {
	t.Parallel()

	for _, a := range gentest.Run(t, servergo.New(), traced(t), opts()) {
		if a.Path == "monitoring.gen.go" {
			t.Error("a project with no monitoring block got a monitoring.gen.go")
		}
		if strings.Contains(string(a.Content), "Monitor") {
			t.Errorf("%s names the page in a project that asked for no page", a.Path)
		}
	}
}

// The block is what rig.yaml said, carried into the one function a main calls.
// The password is not in it: it is read from the environment at run time, the
// same as the collector endpoint and the span file are.
func TestTheGeneratedConfigurationCarriesTheBlock(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), monitored(t), opts()), "monitoring.gen.go")

	for _, want := range []string{
		`ServiceName: "lifecycle"`,
		`Addr:        "127.0.0.1:9090"`,
		`BasePath:    "/_rig/monitor"`,
		`MaxTraces:   200`,
		`MaxLogs:     500`,
		`PasswordEnv: "RIG_MONITOR_PASSWORD"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the generated configuration is missing %s:\n%s", want, src)
		}
	}
	if strings.Contains(src, "Password:") {
		t.Errorf("a project that named a variable got a literal password in its source:\n%s", src)
	}
	if strings.Contains(src, "Allow:") {
		t.Errorf("a project with no allow list got one in its source:\n%s", src)
	}
}

// Which networks may reach the page is a decision about the application, so it
// travels in the generated configuration rather than in the environment.
func TestTheAllowListIsCarriedThrough(t *testing.T) {
	t.Parallel()

	doc := monitored(t)
	doc.API.Monitoring.Allow = []string{"10.0.0.0/8", "127.0.0.1"}

	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "monitoring.gen.go")
	if !strings.Contains(src, `Allow:       []string{"10.0.0.0/8", "127.0.0.1"}`) {
		t.Errorf("the allow list did not reach the generated source:\n%s", src)
	}
}

// A project that wrote the password into rig.yaml — warned about, and its
// decision — gets it in the generated source, because that is where it then
// has to be.
func TestALiteralPasswordIsCarriedThrough(t *testing.T) {
	t.Parallel()

	doc := monitored(t)
	doc.API.Monitoring.Password = "correct horse battery"

	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "monitoring.gen.go")
	if !strings.Contains(src, `Password:    "correct horse battery"`) {
		t.Errorf("the literal password did not reach the generated source:\n%s", src)
	}
}

// The page is not on the API's mux, and there is no field that would put it
// there.
//
// This is the whole of what giving it a listener was for: the address it binds
// to is the only boundary in front of the page that a client cannot talk its
// way around, and a project that mounted it on the API's mux instead would have
// given that up. So the generated server does not offer the choice — Register
// never sees the page, and Server has nowhere to put one.
func TestThePageIsNotOnTheAPIMux(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), monitored(t), opts()), "server.gen.go")

	// "observe" on its own would match the tracing this fixture also has —
	// rig opens a span inside each handler, which is a different thing on the
	// same import.
	for _, unwanted := range []string{"Monitor", "MonitoringPage", "PageConfig"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("the generated server names %q; the page is served on its own listener:\n%s", unwanted, src)
		}
	}
}

// The address is where a main learns to put the listener, so it has to survive
// the trip from rig.yaml into the one function that main calls.
func TestTheAddressIsCarriedThrough(t *testing.T) {
	t.Parallel()

	doc := monitored(t)
	doc.API.Monitoring.Addr = "10.0.0.4:9999"

	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "monitoring.gen.go")
	if !strings.Contains(src, `Addr:        "10.0.0.4:9999"`) {
		t.Errorf("the address did not reach the generated source:\n%s", src)
	}
}
