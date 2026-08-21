// Package reqlog is the response writer a request line is written from.
//
// A handler answers by calling methods on an [net/http.ResponseWriter], and
// what it answered is then gone: the status is in the client's hands and
// nowhere else. A log line that says a request arrived and not how it ended is
// half a line, so something has to remember. This is that something, and it is
// here rather than in the generated code because a wrapper is a place to get
// wrong once rather than once per project.
//
// The two things it must not break are the reason it is not four lines. A
// [Writer] does not implement [net/http.Flusher] or [net/http.Hijacker], so a
// handler that reaches for either by type assertion would stop finding it and
// stop streaming — silently, because a failed assertion is an ok that is false
// and a flush that does not happen. [Writer.Unwrap] is the answer:
// [net/http.ResponseController] follows it to the real writer, which is how
// code should ask for those two anyway. [Writer.ReadFrom] is the same
// consideration for a download, where the interface being hidden is
// [io.ReaderFrom] and what is lost is sendfile.
package reqlog

import (
	"io"
	"net/http"
)

// Writer is an [net/http.ResponseWriter] that remembers what it answered.
type Writer struct {
	http.ResponseWriter

	status int
	bytes  int64
}

// Wrap returns w with a record of its answer attached.
func Wrap(w http.ResponseWriter) *Writer { return &Writer{ResponseWriter: w} }

// WriteHeader records the status and passes it on.
//
// Only the first one counts, which is the same rule net/http itself applies: a
// second WriteHeader is a bug that logs a warning and changes nothing, and a
// record that believed the second would disagree with what the client got.
func (w *Writer) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write records the byte count and passes the bytes on.
func (w *Writer) Write(b []byte) (int, error) {
	w.wroteImplicitly()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile path a download depends on.
//
// [io.Copy] prefers an [io.ReaderFrom] over a plain Write, and the writer
// net/http hands a handler has one. Without this method the copy would find
// the Writer instead, which does not, and every download would go through a
// buffer in userspace for no reason. With it, the bytes go the way they went
// before and the count still happens here.
func (w *Writer) ReadFrom(src io.Reader) (int64, error) {
	w.wroteImplicitly()

	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.bytes += n
		return n, err
	}

	// writerOnly, so the copy cannot find this method again and recurse.
	n, err := io.Copy(writerOnly{w.ResponseWriter}, src)
	w.bytes += n
	return n, err
}

// Unwrap returns the writer underneath.
//
// [net/http.ResponseController] follows it, which is how a handler reaches the
// Flusher and the Hijacker this type deliberately does not forward: forwarding
// them means claiming them, and a Writer that answers to http.Hijacker over a
// connection that cannot be hijacked is worse than one that does not answer.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status is the status the handler answered with.
//
// Zero means it wrote nothing at all — a hijacked connection, or a handler that
// returned without answering. That is a distinct outcome from 200 and it is
// reported as one rather than assumed away.
func (w *Writer) Status() int { return w.status }

// Bytes is how many bytes of body went out.
func (w *Writer) Bytes() int64 { return w.bytes }

// wroteImplicitly records the 200 net/http writes for a handler that sent a
// body without a status of its own.
func (w *Writer) wroteImplicitly() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

// writerOnly hides every method but Write.
type writerOnly struct{ io.Writer }
