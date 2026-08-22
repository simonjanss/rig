//go:build docker

package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// One item's whole life over the wire: created, refused, edited, its history
// read and reverted, retired to the trash, restored. These are the generated
// lifecycle endpoints the front end's detail panel is built on.
func TestAnItemsWholeLife(t *testing.T) {
	api := newServer(t)
	api.seed(t)
	token := api.login(t, SeedEmail)

	var item struct {
		ID     uuid.UUID `json:"id"`
		Title  string    `json:"title"`
		Status string    `json:"status"`
	}

	t.Run("create", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/todos", token: token,
			body: map[string]any{"title": "Walk the whole lifecycle", "status": "todo"},
		})
		if res.status != http.StatusCreated {
			t.Fatalf("create: %d %s", res.status, res.body)
		}
		res.decode(t, &item)
	})

	t.Run("a blank title is refused, per field", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/todos", token: token,
			body: map[string]any{"title": "   "},
		})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("blank title: %d %s, want 422", res.status, res.body)
		}
		var refusal struct {
			Fields struct {
				Title struct {
					Code string `json:"code"`
				} `json:"title"`
			} `json:"fields"`
		}
		res.decode(t, &refusal)
		if refusal.Fields.Title.Code != "CannotBeEmpty" {
			t.Errorf("the refusal should name the field and the code: %s", res.body)
		}
	})

	t.Run("an edit leaves a version behind", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPatch, path: "/api/v1/todos/" + item.ID.String(), token: token,
			body: map[string]any{"title": "Walk the whole lifecycle, slowly"},
		})
		if res.status != http.StatusOK {
			t.Fatalf("update: %d %s", res.status, res.body)
		}

		versions := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos/" + item.ID.String() + "/_versions", token: token})
		var page struct {
			Data []struct {
				ID    uuid.UUID `json:"id"`
				Title string    `json:"title"`
			} `json:"data"`
		}
		versions.decode(t, &page)
		if len(page.Data) != 1 || page.Data[0].Title != "Walk the whole lifecycle" {
			t.Fatalf("the version should be the row as it was: %s", versions.body)
		}

		// Putting it back goes through the ordinary update, so the state being
		// replaced is snapshotted on the way past.
		reverted := api.do(t, request{
			method: http.MethodPost, path: "/api/v1/todos/" + item.ID.String() + "/_revert", token: token,
			body: map[string]any{"versionId": page.Data[0].ID},
		})
		if reverted.status != http.StatusOK {
			t.Fatalf("revert: %d %s", reverted.status, reverted.body)
		}
		var back struct {
			Title string `json:"title"`
		}
		reverted.decode(t, &back)
		if back.Title != "Walk the whole lifecycle" {
			t.Errorf("revert should restore the old title, got %q", back.Title)
		}
	})

	t.Run("delete retires, the trash lists, restore brings back", func(t *testing.T) {
		if res := api.do(t, request{method: http.MethodDelete, path: "/api/v1/todos/" + item.ID.String(), token: token}); res.status != http.StatusNoContent {
			t.Fatalf("delete: %d %s", res.status, res.body)
		}

		// The list stops carrying it; Get by identifier still answers, with the
		// deletion stamped — which is what lets a deep link to a trashed item
		// say so instead of pretending it never existed.
		var live struct {
			Data []struct {
				ID uuid.UUID `json:"id"`
			} `json:"data"`
		}
		api.do(t, request{method: http.MethodGet, path: "/api/v1/todos", token: token}).decode(t, &live)
		for _, row := range live.Data {
			if row.ID == item.ID {
				t.Fatal("a retired row must leave the list")
			}
		}

		deleted := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos/_deleted", token: token})
		if deleted.status != http.StatusOK {
			t.Fatalf("the trash: %d %s", deleted.status, deleted.body)
		}
		var trash struct {
			Data []struct {
				ID uuid.UUID `json:"id"`
			} `json:"data"`
		}
		deleted.decode(t, &trash)
		found := false
		for _, row := range trash.Data {
			found = found || row.ID == item.ID
		}
		if !found {
			t.Fatalf("the retired item should be in the trash: %s", deleted.body)
		}

		restored := api.do(t, request{method: http.MethodPost, path: "/api/v1/todos/" + item.ID.String() + "/_restore", token: token})
		if restored.status != http.StatusOK {
			t.Fatalf("restore: %d %s", restored.status, restored.body)
		}
		if res := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos/" + item.ID.String(), token: token}); res.status != http.StatusOK {
			t.Fatalf("a restored row answers again: %d", res.status)
		}
	})

	t.Run("assign yourself", func(t *testing.T) {
		me := api.accountID(t, uuid.MustParse(SeedTenantID), SeedEmail)
		res := api.do(t, request{
			method: http.MethodPatch, path: "/api/v1/todos/" + item.ID.String(), token: token,
			body: map[string]any{"assigneeAccountId": me},
		})
		if res.status != http.StatusOK {
			t.Fatalf("assign: %d %s", res.status, res.body)
		}
		var got struct {
			AssigneeAccountID *uuid.UUID `json:"assigneeAccountId"`
		}
		res.decode(t, &got)
		if got.AssigneeAccountID == nil || *got.AssigneeAccountID != me {
			t.Errorf("the assignee should be the caller: %s", res.body)
		}
	})
}

// An attachment and its bytes, committed together: the multipart create the
// NOT NULL file column forces, then the download, then the delete.
func TestAttachments(t *testing.T) {
	api := newServer(t)
	api.seed(t)
	token := api.login(t, SeedEmail)

	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: token,
		body: map[string]any{"title": "Item with an attachment"},
	})
	var item struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &item)

	// The row travels as a part named json, the bytes as the file column's
	// field — the same shape the generated clients send.
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	meta, _ := w.CreateFormField("json")
	if _, err := meta.Write([]byte(`{"todoId":"` + item.ID.String() + `","caption":"the plan"}`)); err != nil {
		t.Fatal(err)
	}
	file, _ := w.CreateFormFile("attachmentFile", "plan.txt")
	if _, err := file.Write([]byte("1. drag cards\n2. watch the other window\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	uploaded := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todo-attachments", token: token,
		raw: form.Bytes(), contentType: w.FormDataContentType(),
	})
	if uploaded.status != http.StatusCreated {
		t.Fatalf("create with file: %d %s", uploaded.status, uploaded.body)
	}
	var attachment struct {
		ID               uuid.UUID `json:"id"`
		AttachmentFileID uuid.UUID `json:"attachmentFileId"`
	}
	uploaded.decode(t, &attachment)
	if attachment.AttachmentFileID == uuid.Nil {
		t.Fatalf("the row should carry its file: %s", uploaded.body)
	}

	// The download route is generated from the column: the row, the file, and
	// the name the response should carry.
	downloaded := api.do(t, request{
		method: http.MethodGet, token: token,
		path: "/api/v1/todo-attachments/" + attachment.ID.String() +
			"/attachment-file/" + attachment.AttachmentFileID.String() + "/plan.txt",
	})
	if downloaded.status != http.StatusOK || downloaded.body != "1. drag cards\n2. watch the other window\n" {
		t.Fatalf("download: %d %q", downloaded.status, downloaded.body)
	}

	if res := api.do(t, request{method: http.MethodDelete, path: "/api/v1/todo-attachments/" + attachment.ID.String(), token: token}); res.status != http.StatusNoContent {
		t.Fatalf("delete attachment: %d %s", res.status, res.body)
	}
}
