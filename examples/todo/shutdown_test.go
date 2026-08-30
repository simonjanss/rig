// What a deployment can say about this project's shutdown, run rather than
// compiled.
//
// No database and no build tag, for log_test.go's reason: the generator suites
// compile the emitted code and this runs it. What it is really proving is the
// seam between the two halves — that the struct servergo emits satisfies the
// interface serve declares, and that the names it carries across are the ones
// the wiring registers.
package main

import (
	"testing"
	"time"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/runtime/serve"
)

// The generated set is what the field takes. Nothing else asserts this, and a
// method renamed on either side would otherwise be found by whoever next wrote
// a main function.
var _ serve.Shutdown = api.Shutdown{}

// A field carries across to the name the step is registered under, and a zero
// one carries nothing.
func TestTheShutdownSetNamesTheStepsThisProjectRegisters(t *testing.T) {
	got := api.Shutdown{Notifications: 10 * time.Second}.Steps()

	want := []serve.Step{{Name: "notifications", Timeout: 10 * time.Second}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("Steps() = %v, want %v — and nothing for the field left zero", got, want)
	}
}

// Budget is the total with those numbers in it, which is what a MaxShutdown
// computed rather than copied is written from.
func TestTheShutdownSetsBudgetCountsWhatWasSet(t *testing.T) {
	// 15s for the engine and 5s for the live subscriptions, plus 10s of
	// headroom, is the 30s api.ShutdownBudget states.
	if got := (api.Shutdown{}).Budget(); got != api.ShutdownBudget() {
		t.Errorf("an empty set budgets %s, want the generated %s", got, api.ShutdownBudget())
	}
	if got := (api.Shutdown{Notifications: 10 * time.Second}).Budget(); got != 25*time.Second {
		t.Errorf("budget with a 10s engine = %s, want 25s", got)
	}
}
