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
		BasePath:    "/_rig/monitor",
		MaxTraces:   200,
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
// page gets no page: no file, no field to set, and nothing on the mux.
func TestWithoutTheBlockThereIsNoPage(t *testing.T) {
	t.Parallel()

	for _, a := range gentest.Run(t, servergo.New(), traced(t), opts()) {
		if a.Path == "monitoring.gen.go" {
			t.Error("a project with no monitoring block got a monitoring.gen.go")
		}
		if strings.Contains(string(a.Content), "MonitoringPage") {
			t.Errorf("%s declares the page's interface in a project that asked for no page", a.Path)
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
		`BasePath:    "/_rig/monitor"`,
		`MaxTraces:   200`,
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

// The page is mounted after everything else, so a collision is a panic naming
// rig's own route rather than one this project owns.
func TestThePageIsMountedLast(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), monitored(t), opts()), "server.gen.go")

	monitor := strings.Index(src, "h.Server.Monitor.Mount(mux)")
	auth := strings.Index(src, "h.Server.Auth.Mount(mux)")
	ret := strings.Index(src, "return mux")
	if monitor < 0 {
		t.Fatalf("the page is never mounted:\n%s", src)
	}
	if auth < 0 || monitor < auth {
		t.Error("the page is mounted before the auth routes")
	}
	if ret < 0 || monitor > ret {
		t.Error("the page is mounted after Register has returned its mux")
	}
}
