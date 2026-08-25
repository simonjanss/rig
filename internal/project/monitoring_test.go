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

// monitoringOn is the smallest block that is accepted: the page has a listener
// of its own, and rig defaults neither half of an address.
const monitoringOn = "monitoring:\n  enabled: true\n  addr: 127.0.0.1:9090\n"

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
	_, out := parseMonitoring(t, monitoringOn)
	if !strings.Contains(out, "RIG3005") {
		t.Errorf("monitoring without tracing was accepted:\n%s", out)
	}

	if _, out := parseMonitoring(t, tracingOn+monitoringOn); out != "" {
		t.Errorf("monitoring with tracing was refused:\n%s", out)
	}
}

// Everything the generated wiring passes is resolved here, so that what is in
// the file is what the page is given and there is one place to read it off.
func TestMonitoringDefaults(t *testing.T) {
	p, out := parseMonitoring(t, tracingOn+monitoringOn)
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
	if got.Addr != "127.0.0.1:9090" {
		t.Errorf("address is %q, want the one the file named", got.Addr)
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
		body := tracingOn + monitoringOn + "  " + key + ": -1\n"
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
	p, out := parseMonitoring(t, tracingOn+monitoringOn+"  password: correct horse battery\n")
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

// The one field rig will not pick for you.
//
// A default port is a thing two rig services on a host would fight over, and a
// default interface is a decision about who can reach a page that lists every
// path, request id and error cause the server has seen. Neither is rig's to
// make quietly, so a project that turned the page on and said nothing else is
// refused rather than given one.
func TestThePageNeedsAnAddress(t *testing.T) {
	if _, out := parseMonitoring(t, tracingOn+"monitoring:\n  enabled: true\n"); !strings.Contains(out, "RIG3009") {
		t.Errorf("an enabled page with no address was accepted:\n%s", out)
	}
}

// The shape only, and when rig.yaml is read: a typo here is a server that will
// not start, which is a slow way to learn about a missing colon.
//
// Whether the host resolves and whether the port is free are questions about
// the machine this eventually runs on, and answering them here would be a `rig
// validate` that passes on a laptop and fails in CI for reasons that have
// nothing to do with the project.
func TestTheAddressIsCheckedWhenTheFileIsRead(t *testing.T) {
	for _, addr := range []string{"9090", "127.0.0.1", "127.0.0.1:http-alt", "127.0.0.1:99999"} {
		body := tracingOn + "monitoring:\n  enabled: true\n  addr: " + addr + "\n"
		if _, out := parseMonitoring(t, body); !strings.Contains(out, "RIG3002") {
			t.Errorf("addr %q was accepted:\n%s", addr, out)
		}
	}

	// A bare port is every interface, a name is the machine's to resolve, and
	// zero is a free port somebody wants for a test. All three are things a
	// project may legitimately mean. The quotes on the v6 one are YAML's: an
	// unquoted [ starts a flow sequence.
	for _, addr := range []string{":9090", "127.0.0.1:9090", `"[::1]:9090"`, "monitor.internal:9090", "127.0.0.1:0"} {
		body := tracingOn + "monitoring:\n  enabled: true\n  addr: " + addr + "\n"
		if _, out := parseMonitoring(t, body); out != "" {
			t.Errorf("addr %q was refused:\n%s", addr, out)
		}
	}
}

// `addr` is one of the keys that makes a block configured, so writing only it
// and forgetting `enabled` is refused rather than ignored.
func TestTheAddressCountsAsConfiguring(t *testing.T) {
	body := tracingOn + "monitoring:\n  addr: 127.0.0.1:9090\n"
	if _, out := parseMonitoring(t, body); !strings.Contains(out, "RIG3002") {
		t.Errorf("a block holding only addr was ignored:\n%s", out)
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
	good := tracingOn + monitoringOn + "  allow:\n    - 10.0.0.0/8\n    - 127.0.0.1\n    - ::1\n"
	p, out := parseMonitoring(t, good)
	if out != "" {
		t.Fatalf("this configuration should be valid:\n%s", out)
	}
	if got := p.Config.Monitoring.IR(p.Config.Project.Name).Allow; len(got) != 3 {
		t.Errorf("the list did not reach the document: %v", got)
	}

	bad := tracingOn + monitoringOn + "  allow:\n    - 10.0.0.0/8\n    - office\n"
	if _, out := parseMonitoring(t, bad); !strings.Contains(out, "RIG3002") {
		t.Errorf("an entry that is not an address was accepted:\n%s", out)
	}
}
