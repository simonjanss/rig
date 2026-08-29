package serve

import (
	"errors"
	"testing"
)

// The distinction is what the exit code turns on: a server that never started
// has not done its job, a server that would not close an exporter has.
func TestUncleanSeparatesTidyingFromFailing(t *testing.T) {
	var (
		broken   = errors.New("would not close")
		fatal    = errors.New("could not listen")
		shutdown = &ShutdownError{Err: broken}
	)

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing went wrong", nil, false},
		{"only the tidying", shutdown, true},
		{"tidying, twice", errors.Join(shutdown, &ShutdownError{Err: broken}), true},
		{"wrapped tidying", errors.Join(errors.Join(shutdown)), true},
		{"the server itself", fatal, false},
		{"both", errors.Join(fatal, shutdown), false},
	} {
		if got := Unclean(tc.err); got != tc.want {
			t.Errorf("%s: Unclean = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the cause survives, so a caller that wants the detail has it.
	if !errors.Is(shutdown, broken) {
		t.Error("the failure should still be reachable through the wrapper")
	}
}
