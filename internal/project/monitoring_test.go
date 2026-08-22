package project_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

// parseMonitoring parses a configuration that may be invalid, and returns both
// halves: these tests are mostly about what is refused.
func parseMonitoring(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	src := "project:\n  name: demo\n  module: example.com/demo\n" + body
	p, diags := project.Parse("rig.yaml", []byte(src))
	return p, diags.String()
}

const tracingOn = "tracing:\n  enabled: true\n"

// No block is nil, the same question every generator asks of `tracing:`. A
// block that is present and off would still be a page in the source of a
// project that asked for none.
func TestMonitoringIsNilUntilItIsAskedFor(t *testing.T) {
	for _, body := range []string{"", "monitoring:\n  enabled: false\n"} {
		p, out := parseMonitoring(t, body)
		if out != "" {
			t.Fatalf("this configuration should be valid:\n%s", out)
		}
		if got := p.Config.Monitoring.IR(p.Config.Project.Name); got != nil {
			t.Errorf("monitoring %q resolved to %+v, want nil", body, got)
		}
	}
}

// The page is a reader over the span file. Without tracing it would be empty
// forever, which is a page that looks broken rather than one that is off — so
// the combination is refused when rig.yaml is read rather than discovered by
// somebody staring at an empty list.
func TestThePageNeedsTracing(t *testing.T) {
	_, out := parseMonitoring(t, "monitoring:\n  enabled: true\n")
	if !strings.Contains(out, "RIG3005") {
		t.Errorf("monitoring without tracing was accepted:\n%s", out)
	}

	if _, out := parseMonitoring(t, tracingOn+"monitoring:\n  enabled: true\n"); out != "" {
		t.Errorf("monitoring with tracing was refused:\n%s", out)
	}
}

// Everything the generated wiring passes is resolved here, so that what is in
// the file is what the page is given and there is one place to read it off.
func TestMonitoringDefaults(t *testing.T) {
	p, out := parseMonitoring(t, tracingOn+"monitoring:\n  enabled: true\n")
	if out != "" {
		t.Fatalf("this configuration should be valid:\n%s", out)
	}

	got := p.Config.Monitoring.IR(p.Config.Project.Name)
	if got == nil {
		t.Fatal("an enabled block resolved to nothing")
	}
	if got.ServiceName != "demo" {
		t.Errorf("service name is %q, want the project's %q", got.ServiceName, "demo")
	}
	if got.BasePath != project.DefaultMonitorBasePath {
		t.Errorf("base path is %q, want %q", got.BasePath, project.DefaultMonitorBasePath)
	}
	if got.MaxTraces != project.DefaultMonitorMaxTraces {
		t.Errorf("max traces is %d, want %d", got.MaxTraces, project.DefaultMonitorMaxTraces)
	}
	if got.MaxLogs != project.DefaultMonitorMaxLogs {
		t.Errorf("max logs is %d, want %d", got.MaxLogs, project.DefaultMonitorMaxLogs)
	}
	if got.PasswordEnv != project.DefaultMonitorPasswordEnv {
		t.Errorf("password variable is %q, want %q", got.PasswordEnv, project.DefaultMonitorPasswordEnv)
	}
	if got.Password != "" {
		t.Errorf("a project that wrote no password got %q", got.Password)
	}
}

// Both list sizes are the same kind of number and are checked the same way. A
// negative one is a page that lists nothing, which is not what anybody typing a
// minus sign meant.
func TestTheListSizesHaveToBePositive(t *testing.T) {
	for _, key := range []string{"max_traces", "max_logs"} {
		body := tracingOn + "monitoring:\n  enabled: true\n  " + key + ": -1\n"
		if _, out := parseMonitoring(t, body); !strings.Contains(out, "RIG3002") {
			t.Errorf("a negative %s was accepted:\n%s", key, out)
		}
	}
}

// max_logs is one of the keys that makes a block configured, so writing only it
// and forgetting `enabled` is refused rather than ignored.
func TestMaxLogsCountsAsConfiguring(t *testing.T) {
	if _, out := parseMonitoring(t, tracingOn+"monitoring:\n  max_logs: 50\n"); !strings.Contains(out, "RIG3002") {
		t.Errorf("a block holding only max_logs was ignored:\n%s", out)
	}
}

// A password in rig.yaml is a secret in a file that is checked in, and the page
// it guards lists what every caller did. It is the project's call to make, so
// this warns rather than refusing — but it warns.
func TestALiteralPasswordWarns(t *testing.T) {
	p, out := parseMonitoring(t, tracingOn+"monitoring:\n  enabled: true\n  password: correct horse battery\n")
	if !strings.Contains(out, "RIG3006") {
		t.Errorf("a password in rig.yaml said nothing:\n%s", out)
	}
	if p.Config.Monitoring.Password != "correct horse battery" {
		t.Error("the warning also dropped the password, so the page would not start")
	}
	if strings.Contains(out, "error[") {
		t.Errorf("the warning is an error, and it should be a judgement call:\n%s", out)
	}
}

// Inside the API's own prefix the page occupies a route the project can then
// never have, and net/http would say so as a panic at startup rather than as a
// diagnostic here.
func TestThePageStaysOutOfTheProjectsNamespace(t *testing.T) {
	for _, body := range []string{
		"api:\n  base_path: /api/v1\n" + tracingOn + "monitoring:\n  enabled: true\n  base_path: /api/v1/monitor\n",
		"auth:\n  enabled: true\n  base_path: /auth\n" + tracingOn + "monitoring:\n  enabled: true\n  base_path: /auth/monitor\n",
	} {
		if _, out := parseMonitoring(t, body); !strings.Contains(out, "RIG3002") {
			t.Errorf("a page inside the project's own prefix was accepted:\n%s\n%s", body, out)
		}
	}

	// Whole segments, so a prefix that merely starts the same is not inside it.
	body := "api:\n  base_path: /api\n" + tracingOn + "monitoring:\n  enabled: true\n  base_path: /apiary\n"
	if _, out := parseMonitoring(t, body); out != "" {
		t.Errorf("/apiary was read as being inside /api:\n%s", out)
	}
}

// A block somebody filled in and never enabled reads as a page that is
// configured, and is none: the same failure the auth and files blocks refuse.
func TestAConfiguredPageThatIsNotEnabledIsRefused(t *testing.T) {
	_, out := parseMonitoring(t, tracingOn+"monitoring:\n  max_traces: 50\n")
	if !strings.Contains(out, "RIG3002") {
		t.Errorf("a configured but disabled block was ignored:\n%s", out)
	}
}

// A typo on this list means a page nobody can reach, so it is caught when
// rig.yaml is read rather than when the server tries to start.
func TestTheAllowListIsCheckedWhenTheFileIsRead(t *testing.T) {
	good := tracingOn + "monitoring:\n  enabled: true\n  allow:\n    - 10.0.0.0/8\n    - 127.0.0.1\n    - ::1\n"
	p, out := parseMonitoring(t, good)
	if out != "" {
		t.Fatalf("this configuration should be valid:\n%s", out)
	}
	if got := p.Config.Monitoring.IR(p.Config.Project.Name).Allow; len(got) != 3 {
		t.Errorf("the list did not reach the document: %v", got)
	}

	bad := tracingOn + "monitoring:\n  enabled: true\n  allow:\n    - 10.0.0.0/8\n    - office\n"
	if _, out := parseMonitoring(t, bad); !strings.Contains(out, "RIG3002") {
		t.Errorf("an entry that is not an address was accepted:\n%s", out)
	}
}
