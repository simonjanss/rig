package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/simonjanss/rig/examples/todo/client"
	"github.com/simonjanss/rig/rigclient"
)

// filesDemo moves bytes through a generated client.
//
// Everything a file does that a JSON call does not is here: a body that is a
// stream rather than a struct, a call that has to be bounded differently from
// every other one, a response that is the response rather than a copy of it,
// and a create that commits a row and its bytes together.
func filesDemo(ctx context.Context, args []string) error {
	baseURL := client.DefaultBaseURL
	set := flags("files", args, &baseURL)
	tenant := set.String("tenant", "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"the tenant to act as; examples/todo has no authentication and reads this header")
	if err := set.Parse(args); err != nil {
		return err
	}

	c, err := client.New(rigclient.Config{
		BaseURL:   baseURL,
		Header:    map[string][]string{"X-Tenant-Id": {*tenant}},
		UserAgent: "rig-sdk-demo/1",
	})
	if err != nil {
		return err
	}

	step("Create a todo to hang files off")
	todo, err := c.Todos.Create(ctx, client.TodoCreateInput{Title: "Photograph the summit"})
	if err != nil {
		return err
	}
	detail("id %s", todo.ID)

	// A picture, made here so the demonstration needs nothing on disk. The
	// leading bytes are a real PNG header, which matters: the server decides
	// the type by sniffing the content and ignores what the request claimed.
	cover := pngBytes("cover")

	step("Upload it as the todo's cover")
	detail("The default client times the whole exchange out after thirty seconds.")
	detail("That is right for a JSON call and wrong for anything moving a file, and a")
	detail("context deadline cannot raise it — http.Client.Timeout is a ceiling. So the")
	detail("call is bounded rather than the client.")
	file, err := c.Todos.UploadCoverFile(ctx, todo.ID,
		rigclient.UploadBytes("cover.png", "application/octet-stream", cover),
		rigclient.WithTimeout(10*time.Minute))
	if err != nil {
		return err
	}
	detail("stored %s, %d bytes, sniffed as %s", file.FileName, file.SizeBytes, file.ContentType)
	detail("the request claimed application/octet-stream; the server looked at the bytes")

	step("Read the url off the row")
	detail("The url is on rig_file, written at upload time and stable. Holding it grants")
	detail("nothing — it is unsigned, and the endpoint behind it still checks the caller —")
	detail("which is exactly why it is safe to store and to sync.")
	if file.URL != nil {
		detail("%s", *file.URL)
	}

	step("Download it, and check the bytes came back unchanged")
	content, err := c.Todos.DownloadCoverFile(ctx, todo.ID, file.ID, file.FileName)
	if err != nil {
		return err
	}
	// The one thing the generated method cannot do for you. Nothing reads
	// ahead, which is what lets a file larger than memory go straight to disk —
	// and it means the connection is held until this runs.
	defer content.Body.Close()

	got, err := io.ReadAll(content.Body)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, cover) {
		return fmt.Errorf("the download differs from what went up: %d bytes against %d", len(got), len(cover))
	}
	detail("%d bytes, %s, %s", len(got), content.ContentType, sum(got))
	detail("filename from Content-Disposition: %s", content.Filename)

	step("Ask for part of it, the way a resumed download would")
	partial, err := c.Todos.DownloadCoverFile(ctx, todo.ID, file.ID, file.FileName,
		rigclient.WithRange(8, -1))
	if err != nil {
		return err
	}
	defer partial.Body.Close()

	rest, err := io.ReadAll(partial.Body)
	if err != nil {
		return err
	}
	detail("status %d, %d bytes — a range is a question about this one call, so it is", partial.Status, len(rest))
	detail("a call option rather than a generated parameter")

	step("Ask again with the etag we already have")
	unchanged, err := c.Todos.DownloadCoverFile(ctx, todo.ID, file.ID, file.FileName,
		rigclient.WithIfNoneMatch(content.ETag))
	if err != nil {
		return err
	}
	defer unchanged.Body.Close()
	detail("status %d — you asked the question, so the answer is a result rather than", unchanged.Status)
	detail("an error; on a 304 the body is empty and closing it is all there is to do")

	step("Create an attachment and its file in one request")
	detail("todo_attachment.attachment_file_id is not null, which is only expressible")
	detail("because this exists: the row would otherwise have to be created before the")
	detail("upload had anywhere to go, and a client that made the first request and not")
	detail("the second would leave a caption with no picture.")
	attachment, err := c.TodoAttachments.CreateWithFiles(ctx,
		client.TodoAttachmentCreateInput{TodoID: todo.ID, Caption: rigclient.P("On the summit")},
		client.TodoAttachmentCreateFiles{
			AttachmentFile: rigclient.UploadBytes("summit.png", "image/png", pngBytes("summit")),
		},
		rigclient.WithTimeout(10*time.Minute))
	if err != nil {
		return err
	}
	detail("attachment %s, file %s", attachment.ID, attachment.AttachmentFileID)
	detail("one request, one transaction: the row and the bytes landed together")

	step("Leave a required file out")
	detail("The generated CreateFiles struct makes this a compile error rather than a")
	detail("422, so there is nothing to demonstrate here — which is the point. A")
	detail("nullable file column is a pointer in that struct and a not-null one is not.")

	step("Send something past the cap")
	detail("examples/todo sets files.max_bytes to 5 MiB in rig.yaml, so this is refused")
	detail("rather than truncated. Truncation would look like a successful upload of a")
	detail("file one byte short of the truth.")
	_, err = c.Todos.UploadCoverFile(ctx, todo.ID,
		rigclient.UploadBytes("huge.bin", "application/octet-stream", make([]byte, 6<<20)),
		rigclient.WithTimeout(10*time.Minute))
	switch {
	case err == nil:
		return errors.New("a file past the cap was accepted")
	case statusOf(err) == http.StatusRequestEntityTooLarge:
		detail("refused: %s", err)
	default:
		return err
	}

	step("Remove the cover")
	if err := c.Todos.DeleteCoverFile(ctx, todo.ID); err != nil {
		return err
	}
	again, err := c.Todos.Get(ctx, todo.ID)
	if err != nil {
		return err
	}
	if again.CoverFileID != nil {
		return errors.New("the column was not cleared")
	}
	detail("the column is cleared and the file is retired — its bytes outlive the delete")
	detail("for as long as files.restore_window, because a restore inside that window has")
	detail("to hand back a row pointing at something")

	step("Delete it a second time")
	if err := c.Todos.DeleteCoverFile(ctx, todo.ID); err != nil {
		return err
	}
	detail("not an error: it is already in the state the caller asked for, and answering")
	detail("otherwise would make a retry of a lost response look like a failure")

	fmt.Println()
	return nil
}

// pngBytes is a small file that really is a PNG as far as content sniffing is
// concerned, so the demonstration can show the server disagreeing with what the
// request claimed.
func pngBytes(seed string) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	b.WriteString(seed)
	b.Write(make([]byte, 64))
	return b.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:8])
}

// statusOf is the HTTP status behind a failure, or zero for one that never
// reached the server.
func statusOf(err error) int {
	var e *rigclient.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}
