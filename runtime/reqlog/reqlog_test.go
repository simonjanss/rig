package reqlog_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simonjanss/rig/runtime/reqlog"
)

func TestRecordsTheStatusAndTheBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := reqlog.Wrap(rec)

	w.WriteHeader(http.StatusCreated)
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatalf("write: %v", err)
	}

	if w.Status() != http.StatusCreated {
		t.Errorf("Status() = %d, want %d", w.Status(), http.StatusCreated)
	}
	if w.Bytes() != 5 {
		t.Errorf("Bytes() = %d, want 5", w.Bytes())
	}
	if rec.Body.String() != "hello" {
		t.Errorf("the body did not reach the writer underneath: %q", rec.Body.String())
	}
}

// A handler that writes a body without a status has answered 200, and net/http
// is the one that says so. A record that reported 0 there would make the
// ordinary case look like the one where nothing was written.
func TestABodyWithNoStatusIs200(t *testing.T) {
	w := reqlog.Wrap(httptest.NewRecorder())

	if _, err := io.WriteString(w, "x"); err != nil {
		t.Fatalf("write: %v", err)
	}

	if w.Status() != http.StatusOK {
		t.Errorf("Status() = %d, want %d", w.Status(), http.StatusOK)
	}
}

// Zero is its own outcome: a handler that returned without answering, or one
// that hijacked the connection. Reporting 200 would hide both.
func TestNothingWrittenIsZero(t *testing.T) {
	w := reqlog.Wrap(httptest.NewRecorder())

	if w.Status() != 0 {
		t.Errorf("Status() = %d, want 0", w.Status())
	}
	if w.Bytes() != 0 {
		t.Errorf("Bytes() = %d, want 0", w.Bytes())
	}
}

// net/http warns and ignores a second WriteHeader, so a record that believed
// the second would disagree with what the client actually got.
func TestOnlyTheFirstStatusCounts(t *testing.T) {
	w := reqlog.Wrap(httptest.NewRecorder())

	w.WriteHeader(http.StatusTeapot)
	w.WriteHeader(http.StatusInternalServerError)

	if w.Status() != http.StatusTeapot {
		t.Errorf("Status() = %d, want %d", w.Status(), http.StatusTeapot)
	}
}

// The regression this package exists to not cause. A Writer does not implement
// http.Flusher, so a handler that type-asserts for one stops finding it — which
// is why nothing should, and why ResponseController has to reach through.
func TestResponseControllerFlushesThrough(t *testing.T) {
	var flushed bool
	inner := flusher{ResponseWriter: httptest.NewRecorder(), onFlush: func() { flushed = true }}

	w := reqlog.Wrap(inner)
	if _, ok := any(w).(http.Flusher); ok {
		t.Error("Writer implements http.Flusher; it should hide it and be reached through Unwrap")
	}

	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !flushed {
		t.Error("the flush did not reach the writer underneath")
	}
}

// A download copies into the writer, and io.Copy prefers a ReaderFrom. The
// bytes have to be counted on that path too, or every file download logs zero.
func TestReadFromCountsAndReaches(t *testing.T) {
	rec := httptest.NewRecorder()
	w := reqlog.Wrap(rec)

	n, err := io.Copy(w, plain("twelve bytes"))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != 12 || w.Bytes() != 12 {
		t.Errorf("copied %d, recorded %d, want 12 and 12", n, w.Bytes())
	}
	if rec.Body.String() != "twelve bytes" {
		t.Errorf("the body did not reach the writer underneath: %q", rec.Body.String())
	}
	if w.Status() != http.StatusOK {
		t.Errorf("Status() = %d, want %d", w.Status(), http.StatusOK)
	}
}

// httptest.ResponseRecorder has no ReadFrom, so the copy above already took the
// fallback. This is the other half: a writer that does have one is used rather
// than copied through, which is the whole reason the method exists.
func TestReadFromPrefersTheWriterUnderneath(t *testing.T) {
	var called bool
	inner := readerFrom{ResponseWriter: httptest.NewRecorder(), called: &called}

	w := reqlog.Wrap(inner)
	n, err := io.Copy(w, plain("four"))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !inner.used() {
		t.Error("the underlying ReadFrom was not used, so a download lost its sendfile path")
	}
	if n != 4 || w.Bytes() != 4 {
		t.Errorf("copied %d, recorded %d, want 4 and 4", n, w.Bytes())
	}
}

// plain is a reader and nothing else.
//
// strings.Reader has a WriteTo, and io.Copy prefers that over the destination's
// ReadFrom — so a test that copied from one would take the Write path and prove
// nothing about the method it meant to exercise.
func plain(s string) io.Reader { return readerOnly{strings.NewReader(s)} }

type readerOnly struct{ io.Reader }

type flusher struct {
	http.ResponseWriter
	onFlush func()
}

func (f flusher) Flush() { f.onFlush() }

type readerFrom struct {
	http.ResponseWriter
	called *bool
}

func (r readerFrom) used() bool { return r.called != nil && *r.called }

func (r readerFrom) ReadFrom(src io.Reader) (int64, error) {
	if r.called != nil {
		*r.called = true
	}
	return io.Copy(r.ResponseWriter, src)
}
