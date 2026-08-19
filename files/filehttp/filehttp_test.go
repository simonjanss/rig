// The decisions in this package each have an obvious wrong answer, and a golden
// file would never notice any of them being taken. These are the checks that do.
package filehttp_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/files"
	"github.com/simonjanss/rig/files/filehttp"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// form builds a multipart body with the given parts, in order.
func form(t *testing.T, parts ...[3]string) (string, io.Reader) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		name, filename, content := p[0], p[1], p[2]
		var (
			part io.Writer
			err  error
		)
		if filename == "" {
			part, err = w.CreateFormField(name)
		} else {
			part, err = w.CreateFormFile(name, filename)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), &buf
}

func post(t *testing.T, parts ...[3]string) *http.Request {
	t.Helper()

	ct, body := form(t, parts...)
	r := httptest.NewRequest(http.MethodPost, "/todos/1/cover-file", body)
	r.Header.Set("Content-Type", ct)
	return r
}

// The upload route takes one file under one name, and anything else is a
// mistake worth reporting rather than skipping.
func TestOnePartTakesTheNamedFileAndRefusesAnyOther(t *testing.T) {
	t.Parallel()

	up, err := filehttp.OnePart(post(t, [3]string{"coverFile", "cover.png", "bytes"}), "coverFile")
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "cover.png" {
		t.Errorf("name = %q", up.Name)
	}
	got, _ := io.ReadAll(up.Body)
	if string(got) != "bytes" {
		t.Errorf("body = %q", got)
	}

	_, err = filehttp.OnePart(post(t, [3]string{"covrFile", "cover.png", "x"}), "coverFile")
	if !rigerr.Is(err, rigerr.CodeUnprocessableEntity) {
		t.Errorf("a misspelled part gave %v, want a validation failure — a client "+
			"that uploaded a file into nowhere should not get a 201", err)
	}

	_, err = filehttp.OnePart(post(t), "coverFile")
	if !rigerr.Is(err, rigerr.CodeUnprocessableEntity) {
		t.Errorf("an empty form gave %v, want a validation failure", err)
	}
}

// A JSON body is not a form, and saying so with 415 is what lets a client tell
// "you sent the wrong thing" from "what you sent was wrong".
func TestANonMultipartBodyIsRefusedAsAMediaType(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/todos/1/cover-file", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	_, err := filehttp.OnePart(r, "coverFile")
	if !rigerr.Is(err, rigerr.CodeUnsupportedMediaType) {
		t.Errorf("err = %v, want an unsupported media type", err)
	}
}

// The property that keeps a container's ephemeral disk out of the upload path.
//
// r.ParseMultipartForm spills every part past its memory budget into os.TempDir,
// so a file larger than memory would land there as well as in the store. This
// package uses r.MultipartReader, and nothing should appear.
func TestReadingAFormDoesNotSpillToDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	big := strings.Repeat("x", 8<<20)
	up, err := filehttp.OnePart(post(t, [3]string{"coverFile", "big.bin", big}), "coverFile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, up.Body); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the form left %d files in the temporary directory; it should "+
			"have streamed", len(entries))
	}
}

// The multipart create's two bodies, told apart.
func TestIsMultipartRecognizesAFormAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		contentType string
		want        bool
	}{
		{"multipart/form-data; boundary=abc", true},
		{"multipart/mixed; boundary=abc", true},
		{"application/json", false},
		{"", false},
		{"not a media type at all", false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/todos", nil)
		if tc.contentType != "" {
			r.Header.Set("Content-Type", tc.contentType)
		}
		if got := filehttp.IsMultipart(r); got != tc.want {
			t.Errorf("IsMultipart(%q) = %v, want %v", tc.contentType, got, tc.want)
		}
	}
}

func content(name, contentType, body string) *files.Content {
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	return &files.Content{
		File: &files.File{
			Name: name, ContentType: contentType, Size: int64(len(body)),
			Checksum: "abc123", CreatedAt: at, UploadedAt: &at,
		},
		Body: nopCloser{strings.NewReader(body)},
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// The one that matters. A file served inline from the API origin runs there, and
// the URL is on a row a client syncs — so it reaches an <img> or an <a> without
// anybody thinking about it. text/html downloads.
func TestOnlyAnAllowedTypeIsServedInline(t *testing.T) {
	t.Parallel()

	inline := []string{"image/png"}

	for _, tc := range []struct{ contentType, want string }{
		{"image/png", "inline"},
		{"text/html; charset=utf-8", "attachment"},
		{"application/octet-stream", "attachment"},
		{"image/svg+xml", "attachment"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/f", nil)
		filehttp.Serve(w, r, content("f", tc.contentType, "hello"), inline)

		got := w.Header().Get("Content-Disposition")
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s served as %q, want %s", tc.contentType, got, tc.want)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s was served without nosniff", tc.contentType)
		}
		if w.Header().Get("Content-Type") != tc.contentType {
			t.Errorf("Content-Type = %q, want the type the server decided on: %q",
				w.Header().Get("Content-Type"), tc.contentType)
		}
	}
}

// A name that is not ASCII gets both forms: the plain one for a client that has
// never heard of RFC 5987, and the encoded one beside it.
func TestANonASCIINameTravelsInBothForms(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	filehttp.Serve(w, httptest.NewRequest(http.MethodGet, "/f", nil),
		content("räksmörgås.pdf", "application/pdf", "x"), nil)

	got := w.Header().Get("Content-Disposition")
	if !strings.Contains(got, `filename="r_ksm_rg_s.pdf"`) {
		t.Errorf("no plain filename in %q", got)
	}
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("no RFC 5987 filename in %q", got)
	}
}

// A range request is answered rather than ignored, which is what a resumed
// download and a seeking media element both depend on.
func TestARangeRequestIsAnswered(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/f", nil)
	r.Header.Set("Range", "bytes=2-4")
	filehttp.Serve(w, r, content("f.bin", "application/octet-stream", "0123456789"), nil)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "234" {
		t.Errorf("body = %q, want the requested range", got)
	}
}

// And a conditional one, off the checksum rig computed rather than off whatever
// the backend felt like calling an ETag.
func TestAMatchingETagIsAnsweredWithNotModified(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/f", nil)
	r.Header.Set("If-None-Match", `"abc123"`)
	filehttp.Serve(w, r, content("f.bin", "application/octet-stream", "0123456789"), nil)

	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes", w.Body.Len())
	}
}
