package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// todoDemo walks a todo through its whole life using nothing but the generated
// client.
//
// Worth noticing as it goes: no URL is written down anywhere, the fields are
// checked by the compiler, and the two failures that matter — a conflict and a
// validation failure — are recognized without parsing a message.
func todoDemo(ctx context.Context, args []string) error {
	baseURL := client.DefaultBaseURL
	set := flags("todo", args, &baseURL)
	tenant := set.String("tenant", uuid.NewString(),
		"the tenant to work in; this example reads it from a header")
	if err := set.Parse(args); err != nil {
		return err
	}

	// The whole setup. This example has no authentication — see its main.go —
	// so the tenant travels in a header, which is what Config.Header is for. An
	// API with sessions uses Config.Credential instead; see the auth demo.
	c, err := client.New(rigclient.Config{
		BaseURL:   baseURL,
		Header:    map[string][]string{"X-Tenant-Id": {*tenant}},
		UserAgent: "rig-sdk-demo/1",
	})
	if err != nil {
		return err
	}
	fmt.Printf("talking to %s as tenant %s\n", baseURL, *tenant)

	step("create a todo")
	due := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	notes := "start from an empty directory"
	made, err := c.Todos.Create(ctx, client.TodoCreateInput{
		Title:    "Write the tutorial",
		Notes:    &notes,
		Priority: client.TodoPriorityHigh,
		DueAt:    &due,
	})
	if err != nil {
		return err
	}
	detail("%s  %q, %s, due %s", made.ID, made.Title, made.Priority, made.DueAt.Format(time.DateOnly))

	step("what a validation failure looks like")
	if err := showValidationFailure(ctx, c); err != nil {
		return err
	}

	step("patch one field")
	// Only Title is mentioned, so only Title changes: notes, priority and the
	// due date are left exactly as they were. That distinction is the reason
	// an update input is made of patch wrappers rather than of pointers.
	patched, err := c.Todos.Update(ctx, made.ID, client.TodoUpdateInput{
		Title: patch.NewOptional("Write the tutorial, properly"),
	})
	if err != nil {
		return err
	}
	detail("title is now %q, and notes is still %q", patched.Title, deref(patched.Notes))

	step("clear a nullable field")
	// The other half of the same distinction: absent leaves alone, null clears.
	cleared, err := c.Todos.Update(ctx, made.ID, client.TodoUpdateInput{
		Notes: patch.Null[string](),
	})
	if err != nil {
		return err
	}
	detail("notes is now %v, and the due date survived: %v", cleared.Notes, cleared.DueAt != nil)

	step("search")
	// A search is a QUERY with a body — a read, with a shape the compiler
	// checks. Behind a proxy that refuses the method the client falls back to
	// POST .../_search on its own, once, and remembers.
	found, err := c.Todos.Search(ctx, client.TodoFilter{
		Equals: &client.TodoFilterEquals{Priority: ptr(client.TodoPriorityHigh)},
		Like:   &client.TodoFilterLike{Title: ptr("Write%")},
	}, client.TodoSearchQuery{Limit: rigclient.P(10)})
	if err != nil {
		return err
	}
	detail("%d high-priority todos whose title starts with \"Write\"", found.Pagination.Total)

	step("walk every page")
	// The iterator asks for one page at a time and stops when the total is
	// reached. The error is the second value: a loop that ignores it is a loop
	// that stops early without saying so.
	var seen int
	for todo, err := range c.Todos.All(ctx, client.TodoListQuery{Limit: rigclient.P(2)}) {
		if err != nil {
			return err
		}
		seen++
		if seen <= 3 {
			detail("%s  %q", todo.ID, todo.Title)
		}
	}
	detail("%d in total, two at a time", seen)

	step("a custom endpoint")
	// Nothing about this one is CRUD: it was declared in the table's YAML, and
	// it is a method like any other.
	done, err := c.Todos.Complete(ctx, made.ID, client.TodoCompleteBody{
		Note: ptr("done on the train"),
	})
	if err != nil {
		return err
	}
	detail("done: %v", done.IsDone)

	// Completing twice contradicts the state, and the client says so in a way
	// worth branching on.
	if _, err := c.Todos.Complete(ctx, made.ID, client.TodoCompleteBody{}); err != nil {
		if !rigclient.IsConflict(err) {
			return err
		}
		detail("completing it again is a conflict, as it should be")
	}

	step("history")
	history, err := c.Todos.Versions(ctx, made.ID)
	if err != nil {
		return err
	}
	detail("%d earlier versions kept", history.Pagination.Total)

	step("delete, then restore")
	if err := c.Todos.Delete(ctx, made.ID); err != nil {
		return err
	}
	trash, err := c.Todos.ListDeleted(ctx, client.TodoListDeletedQuery{})
	if err != nil {
		return err
	}
	detail("%d in the trash", trash.Pagination.Total)

	back, err := c.Todos.Restore(ctx, made.ID)
	if err != nil {
		return err
	}
	detail("%s is back, still titled %q", back.ID, back.Title)

	fmt.Println("\ndone.")
	return nil
}

// showValidationFailure sends something the server will refuse, so that the
// interesting half of the client — what a failure looks like — is on screen
// beside the successes.
func showValidationFailure(ctx context.Context, c *client.Client) error {
	_, err := c.Todos.Create(ctx, client.TodoCreateInput{Title: "   "})
	if err == nil {
		detail("the server accepted an empty title, which it was not supposed to")
		return nil
	}
	if !rigclient.IsInvalid(err) {
		return err
	}

	// One line, and the shape is the call's rather than something to name: the
	// envelope and the per-field detail together, shaped like the input that
	// failed, so each message can be put beside the control it belongs to rather
	// than parsed out of a sentence.
	refused, ok := client.TodoCreateError(err)
	if !ok {
		return err
	}
	if refused.Fields != nil && refused.Fields.Title != nil {
		detail("title: %s (%s)", refused.Fields.Title.Message, refused.Fields.Title.Code)
	}
	detail("code %s, status %d, request %s", refused.Code, refused.Status, refused.RequestID)

	if rigclient.CodeOf(err) != rigerr.CodeUnprocessableEntity {
		return err
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
