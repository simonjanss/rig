//go:build docker

// The generated Go client, against the server it was generated from.
//
//	rig db up && go test -tags docker ./...
//
// api_docker_test.go asks whether the server answers correctly, and writes the
// wire shapes out by hand to do it. This asks the other question: whether the
// client and the server agree — the paths, the keys, the statuses, and the two
// places a mismatch would be silent rather than loud. A PATCH that sends a null
// for every field it did not mention still succeeds; it just quietly wipes the
// row. So that one is checked by reading the row back.
package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// newClient points a generated client at a server backed by the real database.
//
// The tenant travels in a header because this example has no authentication —
// see main.go's headerClaims — which is exactly the case Config.Header is for.
func newClient(t *testing.T) (*client.Client, uuid.UUID) {
	t.Helper()

	srv, tenant := newServer(t)

	c, err := client.New(rigclient.Config{
		BaseURL: srv.URL,
		Header:  header("X-Tenant-Id", tenant.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, tenant
}

func header(key, value string) map[string][]string {
	return map[string][]string{key: {value}}
}

func TestClientLifecycle(t *testing.T) {
	c, _ := newClient(t)
	ctx := t.Context()

	due := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	notes := "start from an empty directory"

	created, err := c.Todos.Create(ctx, client.TodoCreateInput{
		Title:    "Write the tutorial",
		Notes:    &notes,
		Priority: client.TodoPriorityHigh,
		DueAt:    &due,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "Write the tutorial" || created.Priority != client.TodoPriorityHigh {
		t.Fatalf("created = %+v", created)
	}
	if created.Notes == nil || *created.Notes != notes {
		t.Errorf("notes = %v, want %q", created.Notes, notes)
	}

	t.Run("get", func(t *testing.T) {
		got, err := c.Todos.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != created.ID {
			t.Errorf("id = %s, want %s", got.ID, created.ID)
		}
	})

	// The one that would be silent. A PATCH naming only the title must leave
	// notes, priority and the due date exactly as they were — which is what
	// omitzero on the patch wrappers buys, and what their MarshalJSON would
	// otherwise undo.
	t.Run("a patch touches only what it names", func(t *testing.T) {
		updated, err := c.Todos.Update(ctx, created.ID, client.TodoUpdateInput{
			Title: patch.NewOptional("Write the tutorial, properly"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Title != "Write the tutorial, properly" {
			t.Errorf("title = %q", updated.Title)
		}
		if updated.Notes == nil || *updated.Notes != notes {
			t.Errorf("notes = %v, want it untouched at %q", updated.Notes, notes)
		}
		if updated.Priority != client.TodoPriorityHigh {
			t.Errorf("priority = %q, want it untouched", updated.Priority)
		}
		if updated.DueAt == nil {
			t.Error("dueAt was cleared by an update that never mentioned it")
		}

		// And read it back, in case the response echoed the request rather than
		// the row.
		reread, err := c.Todos.Get(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reread.Notes == nil || reread.DueAt == nil {
			t.Errorf("the stored row lost fields the update did not mention: %+v", reread)
		}
	})

	// The other half of the same distinction: an explicit null does clear.
	t.Run("a null clears", func(t *testing.T) {
		updated, err := c.Todos.Update(ctx, created.ID, client.TodoUpdateInput{
			Notes: patch.Null[string](),
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Notes != nil {
			t.Errorf("notes = %v, want it cleared", *updated.Notes)
		}
		if updated.DueAt == nil {
			t.Error("dueAt was cleared by an update that only cleared notes")
		}
	})

	t.Run("list and iterate", func(t *testing.T) {
		page, err := c.Todos.List(ctx, client.TodoListQuery{Limit: rigclient.P(1)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Data) != 1 || page.Pagination.Limit != 1 {
			t.Fatalf("page = %+v", page.Pagination)
		}

		// A nil limit is not limit=0: the server's own default applies.
		all, err := c.Todos.List(ctx, client.TodoListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all.Data) == 0 {
			t.Fatal("a query with no limit returned nothing, so the default was not applied")
		}

		// And the iterator walks every page, whatever the page size.
		var seen int
		for todo, err := range c.Todos.All(ctx, client.TodoListQuery{Limit: rigclient.P(1)}) {
			if err != nil {
				t.Fatal(err)
			}
			if todo.ID == uuid.Nil {
				t.Error("a row came back without an identifier")
			}
			seen++
		}
		if int64(seen) != all.Pagination.Total {
			t.Errorf("iterated %d rows, want %d", seen, all.Pagination.Total)
		}
	})

	t.Run("search", func(t *testing.T) {
		found, err := c.Todos.Search(ctx, client.TodoFilter{
			Equals: &client.TodoFilterEquals{Priority: ptr(client.TodoPriorityHigh)},
		}, client.TodoSearchQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(found.Data) == 0 {
			t.Fatal("the high-priority todo did not come back")
		}
		for _, todo := range found.Data {
			if todo.Priority != client.TodoPriorityHigh {
				t.Errorf("%s came back from a filter it does not match", todo.ID)
			}
		}
	})

	t.Run("a custom endpoint is a method like any other", func(t *testing.T) {
		note := "done on the train"
		done, err := c.Todos.Complete(ctx, created.ID, client.TodoCompleteBody{Note: &note})
		if err != nil {
			t.Fatal(err)
		}
		if !done.IsDone {
			t.Error("completing did not mark it done")
		}

		// Completing twice is a conflict rather than a no-op, and the client
		// says which failure it was without anybody parsing prose.
		_, err = c.Todos.Complete(ctx, created.ID, client.TodoCompleteBody{})
		if !rigclient.IsConflict(err) {
			t.Fatalf("err = %v, want a conflict", err)
		}

		var e *rigclient.Error
		if !errors.As(err, &e) || e.Code != rigerr.CodeConflict {
			t.Errorf("err = %v, want a typed conflict", err)
		}
	})

	t.Run("versions and revert", func(t *testing.T) {
		history, err := c.Todos.Versions(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(history.Data) == 0 {
			t.Fatal("a row that has been updated three times has no history")
		}

		oldest := history.Data[len(history.Data)-1]
		back, err := c.Todos.Revert(ctx, created.ID, client.TodoRevertBody{VersionID: oldest.ID})
		if err != nil {
			t.Fatal(err)
		}
		if back.Title != oldest.Title {
			t.Errorf("title = %q, want the reverted %q", back.Title, oldest.Title)
		}
	})

	t.Run("delete and restore", func(t *testing.T) {
		// This example's service refuses to delete an unfinished task, and the
		// revert above put it back to one. Which is worth having in the test:
		// the rule is the application's, and the client neither knows nor needs
		// to know that it exists.
		if _, err := c.Todos.Update(ctx, created.ID, client.TodoUpdateInput{
			IsDone: patch.NewOptional(true),
		}); err != nil {
			t.Fatal(err)
		}

		if err := c.Todos.Delete(ctx, created.ID); err != nil {
			t.Fatal(err)
		}

		// Retired rather than removed: out of the listing, still there by
		// identifier, and in the trash.
		listed, err := c.Todos.List(ctx, client.TodoListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if containsID(listed.Data, created.ID) {
			t.Error("a deleted row is still listed")
		}

		trash, err := c.Todos.ListDeleted(ctx, client.TodoListDeletedQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if !containsID(trash.Data, created.ID) {
			t.Error("the deleted row is not in the trash")
		}

		if _, err := c.Todos.Restore(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Todos.Get(ctx, created.ID); err != nil {
			t.Errorf("the restored row is not readable: %v", err)
		}
	})
}

// A 422 is the one failure with something to say about each field, and the
// generated shape is what makes reading it a struct access rather than a walk
// through map[string]any.
func TestClientValidationFailureIsTyped(t *testing.T) {
	c, _ := newClient(t)

	_, err := c.Todos.Create(t.Context(), client.TodoCreateInput{
		Title:    "   ",
		Priority: client.TodoPriorityHigh,
	})
	if !rigclient.IsInvalid(err) {
		t.Fatalf("err = %v, want a validation failure", err)
	}

	var refused *client.TodoCreateError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the error Create says it returns", err)
	}
	if refused.Fields.Title == nil {
		t.Fatalf("nothing was said about the title: %+v", refused.Fields)
	}
	if refused.Fields.Title.Code != rigerr.FieldCodeCannotBeEmpty {
		t.Errorf("title code = %q, want CannotBeEmpty", refused.Fields.Title.Code)
	}

	// The envelope rides on the same value, which is the difference between one
	// match and two. The request id is empty here because this server sets no
	// RequestID hook, and that is the server's choice rather than the client's.
	if refused.Code != rigerr.CodeUnprocessableEntity ||
		refused.Status != http.StatusUnprocessableEntity {
		t.Errorf("code = %q, status = %d, want both off the same value",
			refused.Code, refused.Status)
	}
}

// A search issues QUERY and falls back to the POST alias when something refuses
// the method. Both routes are the server's, so both are checked here rather
// than against a stub.
func TestClientSearchWorksBothWays(t *testing.T) {
	srv, tenant := newServer(t)

	direct, err := client.New(rigclient.Config{
		BaseURL: srv.URL,
		Header:  header("X-Tenant-Id", tenant.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Todos.Create(t.Context(), client.TodoCreateInput{
		Title: "searchable", Priority: client.TodoPriorityLow,
	}); err != nil {
		t.Fatal(err)
	}

	// QUERY, as the client prefers it.
	found, err := direct.Todos.Search(t.Context(), client.TodoFilter{
		Like: &client.TodoFilterLike{Title: ptr("search%")},
	}, client.TodoSearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Data) == 0 {
		t.Fatal("QUERY found nothing")
	}

	// And through a proxy that has never heard of the method.
	refuser := httptest.NewServer(refuseQuery(srv))
	t.Cleanup(refuser.Close)

	behindProxy, err := client.New(rigclient.Config{
		BaseURL: refuser.URL,
		Header:  header("X-Tenant-Id", tenant.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	viaPost, err := behindProxy.Todos.Search(t.Context(), client.TodoFilter{
		Like: &client.TodoFilterLike{Title: ptr("search%")},
	}, client.TodoSearchQuery{})
	if err != nil {
		t.Fatalf("the fallback did not work: %v", err)
	}
	if len(viaPost.Data) != len(found.Data) {
		t.Errorf("the fallback found %d rows, QUERY found %d", len(viaPost.Data), len(found.Data))
	}
}

func containsID(rows []client.Todo, id uuid.UUID) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }

// refuseQuery stands in for the intermediary this whole fallback exists for: a
// proxy that answers 405 to a method it has never heard of, and passes
// everything else through.
func refuseQuery(target *httptest.Server) http.Handler {
	upstream, err := url.Parse(target.URL)
	if err != nil {
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "QUERY" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
