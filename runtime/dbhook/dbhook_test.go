package dbhook_test

import (
	"context"
	"errors"
	"testing"

	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// This package is the contract between generated repositories and hand-written
// services: the repository calls these fields, the service fills them in, and
// nothing here has behavior of its own. What is worth pinning is the shape —
// a signature that changes silently breaks every generated repository in every
// project, and the compiler is the only thing that would notice.

type todo struct {
	Title string
	Done  bool
}

type todoCreate struct{ Title string }

type todoUpdate struct{ Done *bool }

// validator stands in for the generated model validator, which is the only
// implementation of these interfaces that exists in a real project.
type validator struct{ err error }

func (v validator) RunCreate(context.Context, tenancy.Claims, *todoCreate) error { return v.err }

func (v validator) RunUpdate(context.Context, tenancy.Claims, *todoUpdate, *todo) error { return v.err }

var (
	_ dbhook.CreateValidator[todoCreate]       = validator{}
	_ dbhook.UpdateValidator[todoUpdate, todo] = validator{}
	_ dbhook.CreateValidator[todoCreate]       = (*validator)(nil)
	_ dbhook.UpdateValidator[todoUpdate, todo] = (*validator)(nil)
)

// The common case is a write with no rules attached at all, and a repository
// that had to be told so would put a nil check in every caller instead of one
// in itself.
func TestTheZeroHookSetIsAWriteWithNothingAroundIt(t *testing.T) {
	t.Parallel()

	create := dbhook.Create[todoCreate, todo]{Input: todoCreate{Title: "buy milk"}}
	if create.Hooks.Validator != nil || create.Hooks.Before != nil ||
		create.Hooks.After != nil || create.Hooks.AfterCommit != nil {
		t.Errorf("the zero create carries hooks: %+v", create.Hooks)
	}
	if create.Input.Title != "buy milk" {
		t.Errorf("Input = %+v", create.Input)
	}

	update := dbhook.Update[todoUpdate, todo]{}
	if update.Hooks.Validator != nil || update.Hooks.Before != nil {
		t.Errorf("the zero update carries hooks: %+v", update.Hooks)
	}

	// Delete and Restore have no validator at all: they carry an identifier and
	// a flag, and there is nothing about either for a field rule to check. A
	// reason to refuse one goes in Before.
	del := dbhook.Delete[todoCreate, todo]{}
	if del.Hooks.Before != nil || del.Hooks.After != nil || del.Hooks.AfterCommit != nil {
		t.Errorf("the zero delete carries hooks: %+v", del.Hooks)
	}
	restore := dbhook.Restore[todoCreate, todo]{}
	if restore.Hooks.Before != nil {
		t.Errorf("the zero restore carries hooks: %+v", restore.Hooks)
	}
}

// Before runs on a pointer to the input, so what it changes is what gets
// written. A hook that could only inspect the input would leave a service
// stamping defaults in the caller instead.
func TestBeforeCanChangeWhatIsWritten(t *testing.T) {
	t.Parallel()

	create := dbhook.Create[todoCreate, todo]{
		Input: todoCreate{Title: "  buy milk  "},
		Hooks: dbhook.CreateHooks[todoCreate, todo]{
			Before: func(_ context.Context, _ tenancy.Claims, in *todoCreate) error {
				in.Title = "buy milk"
				return nil
			},
		},
	}

	if err := create.Hooks.Before(t.Context(), tenancy.Claims{}, &create.Input); err != nil {
		t.Fatal(err)
	}
	if create.Input.Title != "buy milk" {
		t.Errorf("Input = %+v, want the change Before made", create.Input)
	}
}

// An update rule regularly needs the row as it was — a status that may only
// move forward is the usual one — which is why the two validators are
// different types rather than one with a spare parameter.
func TestAnUpdateValidatorSeesTheRowAsItWas(t *testing.T) {
	t.Parallel()

	refused := errors.New("a finished todo cannot be reopened")

	var seen *todo
	hooks := dbhook.UpdateHooks[todoUpdate, todo]{
		Validator: updateFunc(func(_ context.Context, _ tenancy.Claims, in *todoUpdate, prev *todo) error {
			seen = prev
			if prev.Done && in.Done != nil && !*in.Done {
				return refused
			}
			return nil
		}),
	}

	prev := &todo{Title: "buy milk", Done: true}
	reopen := false

	err := hooks.Validator.RunUpdate(t.Context(), tenancy.Claims{}, &todoUpdate{Done: &reopen}, prev)
	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want the transition to be refused", err)
	}
	if seen != prev {
		t.Error("the validator should be handed the row it is deciding about")
	}
}

type updateFunc func(context.Context, tenancy.Claims, *todoUpdate, *todo) error

func (f updateFunc) RunUpdate(ctx context.Context, claims tenancy.Claims, in *todoUpdate, prev *todo) error {
	return f(ctx, claims, in, prev)
}
