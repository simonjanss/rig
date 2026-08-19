package rigerr_test

import (
	"errors"
	"fmt"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// A service returns a coded error; the generated handler asks it for a status
// and a code without knowing which rule produced it.
func ExampleNotFound() {
	err := rigerr.NotFound("no todo with id %q", "t-42")

	fmt.Println(err)
	fmt.Println(rigerr.CodeOf(err), rigerr.StatusOf(err))

	// Output:
	// NotFound: no todo with id "t-42"
	// NotFound 404
}

// The code survives wrapping, which is what makes it safe for a service to add
// context on the way out. A handler reads the code off whatever it was handed,
// however many layers added to it.
func ExampleCodeOf() {
	err := rigerr.Conflict("that slug is taken")
	wrapped := fmt.Errorf("create the project: %w", err)

	fmt.Println(rigerr.CodeOf(wrapped), rigerr.StatusOf(wrapped))
	fmt.Println(rigerr.Is(wrapped, rigerr.CodeConflict))
	fmt.Println(errors.Is(wrapped, err))

	// Output:
	// Conflict 409
	// true
	// true
}

// An error that never passed through this package is a server problem until
// somebody decides otherwise, so it reads as Internal rather than as something
// the caller could fix.
func ExampleCodeOf_uncoded() {
	fmt.Println(rigerr.CodeOf(errors.New("the connection went away")))
	fmt.Println(rigerr.StatusOf(errors.New("the connection went away")))

	// Output:
	// Internal
	// 500
}

// Internal keeps the cause for the log and shows the caller only the message.
// The detail of an internal failure is exactly the kind of thing that leaks a
// table name or a connection string.
func ExampleInternal() {
	cause := errors.New("dial tcp 10.0.0.4:5432: connect: connection refused")
	err := rigerr.Internal(cause, "load the todo list")

	fmt.Println(err)                   // what a log gets
	fmt.Println(err.Code, err.Message) // what a client gets
	fmt.Println(errors.Is(err, cause)) // the cause is still reachable

	// Output:
	// Internal: load the todo list: dial tcp 10.0.0.4:5432: connect: connection refused
	// Internal load the todo list
	// true
}
