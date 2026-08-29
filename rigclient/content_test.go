// Files: the multipart body going out, and the bytes coming back.
//
// Hermetic, like the rest of this package's tests. The handlers here read a form
// with r.MultipartReader rather than r.ParseMultipartForm, because that is what
// the generated server does — a test that parsed the easy way would pass on a
// body the real server rejects.
package rigclient_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/rigclient"
)

// part is what a handler read off the wire.
type part struct {
	name        string
	filename    string
	contentType string
	body        string
}

// readParts consumes a multipart request the way the generated server does.
func readParts(t *testing.T, r *http.Request) []part {
	t.Helper()

	mr, err := r.MultipartReader()
	if err != nil {
		t.Errorf("MultipartReader: %v", err)
		return nil
	}

	var out []part
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Errorf("NextPart: %v", err)
			return out
		}
		body, err := io.ReadAll(p)
		if err != nil {
			t.Errorf("reading the part %q: %v", p.FormName(), err)
			return out
		}
		out = append(out, part{
			name:        p.FormName(),
			filename:    p.FileName(),
			contentType: p.Header.Get("Content-Type"),
			body:        string(body),
		})
	}
}

// The order is the contract: the server reads the parts as a stream, so it has
// to meet the row before it meets the bytes that belong to it.
func TestTheJSONPartComesFirst(t *testing.T) {
	var got []part
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readParts(t, r)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todo-attachments",
		Multipart: &rigclient.Multipart{
			JSON: map[string]any{"caption": "on the summit"},
			Files: []rigclient.Upload{
				rigclient.Part("attachmentFile",
					rigclient.UploadBytes("summit.jpg", "image/jpeg", []byte("bytes"))),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d parts, want the json part and one file", len(got))
	}
	if got[0].name != "json" {
		t.Errorf("the first part is %q, want json", got[0].name)
	}
	if !strings.Contains(got[0].body, "on the summit") {
		t.Errorf("the json part is %q, want the row in it", got[0].body)
	}
	if got[1].name != "attachmentFile" || got[1].body != "bytes" {
		t.Errorf("the file part is %q = %q, want attachmentFile = bytes",
			got[1].name, got[1].body)
	}
}

// A bare upload has no row to send, so there is no json part at all — and a
// server reading the parts in order must not be handed an empty one.
func TestAnUploadWithNoRowSendsNoJSONPart(t *testing.T) {
	var got []part
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readParts(t, r)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.UploadBytes("c.png", "image/png", []byte("x"))),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].name != "coverFile" {
		t.Fatalf("got %+v, want only the file part", got)
	}
}

// multipart.Writer.CreateFormFile would have written application/octet-stream
// here whatever the caller said, which is why the header is built by hand.
func TestTheDeclaredContentTypeReachesThePart(t *testing.T) {
	var got []part
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readParts(t, r)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile",
				rigclient.UploadBytes(`a "quoted" name.png`, "image/png", []byte("x"))),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d parts, want one", len(got))
	}
	if got[0].contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got[0].contentType)
	}
	if want := `a "quoted" name.png`; got[0].filename != want {
		t.Errorf("filename = %q, want %q — the quotes were not escaped", got[0].filename, want)
	}
}

// An application proxying an upload passes through a name somebody else chose,
// so a newline in one must not be able to end the part's header and start
// whatever came after it. mime/multipart's own escaper does not do this.
func TestANameCannotInjectAHeader(t *testing.T) {
	var got []part
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = readParts(t, r)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.UploadBytes(
				"evil\r\nContent-Type: text/html\r\n\r\n<script>", "image/png", []byte("x"))),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d parts, want one: the name split the form", len(got))
	}
	if strings.ContainsAny(got[0].filename, "\r\n") {
		t.Errorf("filename = %q, want the line breaks gone", got[0].filename)
	}
	if got[0].contentType != "image/png" {
		t.Errorf("Content-Type = %q, want image/png — the name overwrote it",
			got[0].contentType)
	}
	if got[0].body != "x" {
		t.Errorf("body = %q, want x", got[0].body)
	}
}

// The observable consequence of streaming: the length is not known when the
// headers are written, so the body is chunked. A test asserting the bytes never
// landed in memory cannot be written; this is the fact that follows from it.
func TestAFormIsStreamedRatherThanBuffered(t *testing.T) {
	var length int64
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		length = r.ContentLength
		readParts(t, r)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.UploadBytes("c.png", "image/png", []byte("x"))),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if length != -1 {
		t.Errorf("Content-Length = %d, want it unset: a buffered form is the thing "+
			"the pipe exists to avoid", length)
	}
}

// Both is a generated method with a bug in it, and saying so beats picking one.
func TestAJSONBodyAndAFormTogetherAreRefused(t *testing.T) {
	var calls int
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos",
		Body:      map[string]any{"title": "x"},
		Multipart: &rigclient.Multipart{},
	})
	if err == nil {
		t.Fatal("an Op carrying both a body and a form was sent")
	}
	if calls != 0 {
		t.Errorf("the server saw %d requests, want none", calls)
	}
}

// refresher is a credential that answers a 401 once, the way a Session does.
type refresher struct {
	token string
	calls int
}

func (c *refresher) Apply(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+c.token)
	return nil
}

func (c *refresher) Reauthorize(context.Context) (bool, error) {
	c.calls++
	c.token = "fresh"
	return true, nil
}

// A file on disk seeks, which is the case: the retry re-reads it from the start
// and the server sees the whole thing the second time.
func TestASeekableUploadIsSentAgainAfterARefresh(t *testing.T) {
	var bodies []string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := readParts(t, r)
		if len(parts) == 1 {
			bodies = append(bodies, parts[0].body)
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{Credential: &refresher{token: "stale"}})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile",
				rigclient.UploadBytes("c.png", "image/png", []byte("the bytes"))),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(bodies) != 2 {
		t.Fatalf("the server read %d bodies, want the first and the retry", len(bodies))
	}
	if bodies[0] != "the bytes" || bodies[1] != "the bytes" {
		t.Errorf("bodies = %q, want the same bytes twice", bodies)
	}
}

// A caller who seeks into a file and uploads the tail of it means the tail. A
// retry that helpfully rewound to the start would send bytes nobody asked for,
// and would send them silently.
func TestARetrySendsWhatTheFirstAttemptSentAndNoMore(t *testing.T) {
	var bodies []string
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := readParts(t, r)
		if len(parts) == 1 {
			bodies = append(bodies, parts[0].body)
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{Credential: &refresher{token: "stale"}})

	// Positioned past the header, the way a caller uploading the tail of a log
	// would leave it.
	body := bytes.NewReader([]byte("HEADERthe tail"))
	if _, err := body.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.Upload{Name: "log.txt", Body: body}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(bodies) != 2 {
		t.Fatalf("the server read %d bodies, want the first and the retry", len(bodies))
	}
	for i, got := range bodies {
		if got != "the tail" {
			t.Errorf("body %d = %q, want %q: the retry did not start where the "+
				"first attempt did", i, got, "the tail")
		}
	}
}

// The crux. A pipe cannot be read twice, and buffering every upload so that a
// rare retry is always possible is the trade this refuses to make — so the
// caller is told, and told both halves of why.
func TestAnUploadThatCannotSeekRefusesTheRetry(t *testing.T) {
	var requests int
	cred := &refresher{token: "stale"}
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
	}), rigclient.Config{Credential: cred})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			// A reader with no Seek, which is what a pipe or another response is.
			rigclient.Part("coverFile", rigclient.Upload{
				Name: "c.png", ContentType: "image/png",
				Body: struct{ io.Reader }{strings.NewReader("x")},
			}),
		}},
	})

	if !errors.Is(err, rigclient.ErrCannotRetry) {
		t.Errorf("err = %v, want ErrCannotRetry", err)
	}
	if !rigclient.IsUnauthorized(err) {
		t.Errorf("err = %v, want it to still answer IsUnauthorized: a caller needs "+
			"both facts to decide what to do", err)
	}
	if requests != 1 {
		t.Errorf("the server saw %d requests, want one", requests)
	}
	if cred.calls != 0 {
		t.Errorf("the credential refreshed %d times, want none: there was nothing "+
			"the refresh could have been for", cred.calls)
	}
}

func TestADownloadCarriesTheBytesAndWhatTheyAre(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "*/*" {
			t.Errorf("Accept = %q, want */*: a download is not JSON", got)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("ETag", `"v1"`)
		// RFC 5987, which is what the server sends for a name that is not ASCII.
		w.Header().Set("Content-Disposition",
			`attachment; filename="resume.pdf"; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`)
		w.Write([]byte("bytes"))
	}), rigclient.Config{})

	got, err := rigclient.DoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1/cover-file/2/resume.pdf",
		Accept: "*/*",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "bytes" {
		t.Errorf("body = %q, want bytes", body)
	}
	if got.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", got.ContentType)
	}
	if got.Length != 5 {
		t.Errorf("Length = %d, want 5", got.Length)
	}
	if got.ETag != `"v1"` {
		t.Errorf("ETag = %q, want the server's", got.ETag)
	}
	if want := "résumé.pdf"; got.Filename != want {
		t.Errorf("Filename = %q, want %q — the RFC 5987 form was not read",
			got.Filename, want)
	}
	if got.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", got.Status)
	}
}

// A download that fails is still a rig failure, and a caller switching on the
// code must not have to tell a JSON error page from an image first.
func TestADownloadFailureIsATypedError(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"NotFound","message":"no such file"}`))
	}), rigclient.Config{})

	got, err := rigclient.DoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1/cover-file/2/x.png", Accept: "*/*",
	})
	if got != nil {
		t.Error("a refused download came back with a body to read")
	}
	if !rigclient.IsNotFound(err) {
		t.Errorf("err = %v, want a not-found", err)
	}
}

func TestARangeIsAskedForAndAnsweredAsOne(t *testing.T) {
	const file = "0123456789"

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Range"), "bytes=2-4"; got != want {
			t.Errorf("Range = %q, want %q", got, want)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 2-4/%d", len(file)))
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(file[2:5]))
	}), rigclient.Config{})

	got, err := rigclient.DoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1/cover-file/2/x.bin", Accept: "*/*",
	}, rigclient.WithRange(2, 4))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.Status != http.StatusPartialContent {
		t.Errorf("Status = %d, want 206", got.Status)
	}
	body, _ := io.ReadAll(got.Body)
	if string(body) != "234" {
		t.Errorf("body = %q, want 234", body)
	}
}

// The answer to a question this caller asked, so it comes back as a result and
// not as an error. For anybody who did not ask, a 304 is still a failure.
func TestANotModifiedIsAResultForTheCallerWhoAskedForIt(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusNotModified)
	})

	rt := newClient(t, handler, rigclient.Config{})
	op := rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1/cover-file/2/x.png", Accept: "*/*",
	}

	got, err := rigclient.DoContent(t.Context(), rt, op, rigclient.WithIfNoneMatch(`"v1"`))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.Status != http.StatusNotModified {
		t.Errorf("Status = %d, want 304", got.Status)
	}
	body, _ := io.ReadAll(got.Body)
	if len(body) != 0 {
		t.Errorf("body = %q, want nothing", body)
	}

	if _, err := rigclient.DoContent(t.Context(), rt, op); err == nil {
		t.Error("a 304 nobody asked for came back as a success")
	}
}

// A 204 is a download of nothing, and nil is how every other verb here says so.
func TestADownloadWithNoContentIsNil(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	got, err := rigclient.DoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos/1/cover-file/2/x.png", Accept: "*/*",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// A form that fails halfway through has to arrive as that failure. Without
// CloseWithError on the pipe it arrives as a body that simply stopped, and the
// real cause is nowhere.
func TestAFailureMidFormIsReported(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{})

	boom := errors.New("the disk went away")
	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.Upload{
				Name: "c.png",
				Body: io.MultiReader(strings.NewReader("some"), errReader{boom}),
			}),
		}},
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the reader's own failure", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// The upload the generated client will make, end to end against a handler that
// reads it the way the generated server does.
func TestAnUploadRoundTrips(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		p, err := mr.NextPart()
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := io.ReadAll(p)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"file-1","name":%q,"size":%d}`, p.FileName(), len(body))
	}), rigclient.Config{})

	type file struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Size int    `json:"size"`
	}

	got, err := rigclient.Do[file](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.Upload{
				Name: "cover.png", ContentType: "image/png",
				Body: bytes.NewReader([]byte("0123456789")),
			}),
		}},
	}, rigclient.WithTimeout(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	if got.Name != "cover.png" || got.Size != 10 {
		t.Errorf("got %+v, want the name and size the server read back", got)
	}
}

// A search is idempotent whatever it carries, so a QUERY with a form in it is
// repeatable — and the retry has to put the body back where it started before
// waiting, not after. This is the rewind on the ordinary retry path; the other
// tests here take the one on the reauthorization path.
func TestARepeatableFormIsRewoundBeforeTheRetry(t *testing.T) {
	var bodies []string
	var seen int
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		parts := readParts(t, r)
		if len(parts) == 1 {
			bodies = append(bodies, parts[0].body)
		}
		if seen == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"code":"Internal","message":"not now"}`))
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: rigclient.Retry{Base: time.Millisecond, Cap: time.Millisecond}})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("filter", rigclient.UploadBytes("filter.json", "application/json",
				[]byte(`{"title":"x"}`))),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if len(bodies) != 2 {
		t.Fatalf("the server read %d bodies, want the first and the retry", len(bodies))
	}
	for i, got := range bodies {
		if got != `{"title":"x"}` {
			t.Errorf("body %d = %q, want the form rewound to where it started", i, got)
		}
	}
}
