package project_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

func parseTracing(t *testing.T, body string) *project.Project {
	t.Helper()

	src := "project:\n  name: demo\n  module: example.com/demo\n" + body
	p, diags := project.Parse("rig.yaml", []byte(src))
	if out := diags.String(); out != "" {
		t.Fatalf("this configuration should be valid:\n%s", out)
	}
	return p
}

// No block is nil, which is the question every generator asks. Anything else —
// a block that is present and off — would put rig/observe in the go.mod of a
// project that asked for no spans.
func TestTracingIsNilUntilItIsAskedFor(t *testing.T) {
	for _, body := range []string{"", "tracing:\n  enabled: false\n"} {
		p := parseTracing(t, body)
		if got := p.Config.Tracing.IR(p.Config.Project.Name); got != nil {
			t.Errorf("tracing %q resolved to %+v, want nil", body, got)
		}
	}
}

// The service name is the project's. There is no key here to disagree with it,
// which is the point: two names for one application is the kind of thing nobody
// notices until a collector has both.
func TestTracingTakesTheProjectsName(t *testing.T) {
	p := parseTracing(t, "tracing:\n  enabled: true\n")

	got := p.Config.Tracing.IR(p.Config.Project.Name)
	if got == nil {
		t.Fatal("an enabled block resolved to nothing")
	}
	if !got.Enabled {
		t.Error("resolved block is not enabled")
	}
	if got.ServiceName != "demo" {
		t.Errorf("service name is %q, want the project's %q", got.ServiceName, "demo")
	}
}
