package serve

import "errors"

// ShutdownError reports that the process stopped, but did not tidy up cleanly:
// a dependency that would not close, a flush that did not finish.
//
// It is a distinct type because the two failures deserve different answers. A
// server that could not start has not done its job, and a non-zero exit is how
// it says so. A server that served for a week and then failed to close an
// exporter has done its job, and exiting non-zero would mark an ordinary
// rollout as a crashed container in every dashboard that counts them.
type ShutdownError struct{ Err error }

func (e *ShutdownError) Error() string { return "shutdown: " + e.Err.Error() }
func (e *ShutdownError) Unwrap() error { return e.Err }

// Unclean reports whether err is only about a shutdown that did not go well.
//
// Only: a startup failure joined with a failed teardown is still a startup
// failure, and still fatal.
func Unclean(err error) bool { return err != nil && onlyShutdown(err) }

func onlyShutdown(err error) bool {
	// The switch walks the tree itself, one node at a time, which is the whole
	// point: errors.As answers "is there a ShutdownError anywhere in here", and
	// the question is whether there is anything else.
	switch e := err.(type) { //nolint:errorlint // this walks the wrapping itself
	case *ShutdownError:
		return true
	case interface{ Unwrap() []error }:
		// errors.Join. Every branch has to be a shutdown failure for the whole
		// to be one.
		for _, sub := range e.Unwrap() {
			if !onlyShutdown(sub) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyShutdown(errors.Unwrap(err))
	default:
		return false
	}
}
