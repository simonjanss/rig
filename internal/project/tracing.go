package project

import "github.com/simonjanss/rig/pkg/ir"

// IR is the resolved block, as a document carries it.
//
// Nil for a project that asked for no spans, so that a generator asks the
// document one question — does this project trace — rather than reading a flag
// and then deciding what it implies. That nil is what keeps rig/observe, and
// with it OpenTelemetry, out of an application that never asked for either.
//
// The service name comes from the project rather than from this block. There is
// no `service_name` key to disagree with `project.name`, which is the kind of
// disagreement nobody notices until two deployments arrive in a collector under
// names neither of them recognises.
//
// There is no checkTracing beside this, and nothing in applyDefaults. With one
// key there is nothing to refuse and nothing to fill in: a `tracing:` block that
// is off is a block that says what every project without one says. A check that
// cannot fire is worse than no check, because it reads like coverage.
func (t Tracing) IR(serviceName string) *ir.Tracing {
	if !t.Enabled {
		return nil
	}
	return &ir.Tracing{Enabled: true, ServiceName: serviceName}
}
