package rigclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// Multipart is a multipart/form-data body: the row, and the files beside it.
//
// It is the shape rig's own endpoints take rather than a general form encoder.
// The JSON part is written first and is named "json", because the server reads
// the parts in order and needs the row before it has anywhere to put the bytes —
// which is also why Files is a slice and not a map.
//
// A generated method builds this; a caller supplies the [Upload] that goes in
// it. See [Op.Multipart].
type Multipart struct {
	// JSON is encoded into the part named "json". Nil leaves the part out,
	// which is what a bare upload does: there is no row to send, only bytes for
	// a row that already exists.
	JSON any

	// Files are the file parts, in the order they are written.
	Files []Upload
}

// Upload is one file a caller is sending.
type Upload struct {
	// Field is the part's name, which is the field the server binds the bytes
	// to. A generated method fills it in from the document, through [Part];
	// setting it by hand is for a client somebody assembled themselves.
	Field string

	// Name is what the file is called. The server records it on the row and puts
	// it in the download path, and it never becomes the storage key — so a name
	// with a slash in it is a strange name and not a way out of the bucket.
	Name string

	// ContentType is what the caller claims the bytes are. Empty sends none, and
	// it makes little difference either way: the server sniffs the content and
	// the sniffed type is the one it stores and the one it serves back.
	ContentType string

	// Body is the bytes. A reader that can also seek can be sent twice, which is
	// what a credential refresh mid-upload needs — see [ErrCannotRetry].
	Body io.Reader
}

// Part names an upload, for a generated method putting a caller's file into the
// field the document says it belongs in.
//
// The caller supplies the name, the type and the bytes; only the generated
// method knows what the part is called on the wire, the same way only it knows
// which argument goes in which path segment.
func Part(field string, u Upload) Upload {
	u.Field = field
	return u
}

// UploadBytes is a file already in memory.
//
// It is also the form that can always be sent twice, because a reader over a
// byte slice can seek. See [ErrCannotRetry].
func UploadBytes(name, contentType string, b []byte) Upload {
	return Upload{Name: name, ContentType: contentType, Body: bytes.NewReader(b)}
}

// ErrCannotRetry is returned when a request had to be sent a second time and
// its body could not be produced again.
//
// It is only ever an upload from a reader that cannot seek — a pipe, another
// response, a decompressor. The alternative is to buffer every upload so that a
// retry which almost never happens is always possible, and that is the wrong
// trade for a feature whose whole point is that a large file never lands in
// memory.
//
// It arrives joined to the failure that prompted the retry, so an upload
// interrupted by an expired credential still answers [IsUnauthorized]. A caller
// needs both facts: the credential has been refreshed, and the call has to be
// made again from a reader that still has the bytes.
var ErrCannotRetry = errors.New("rigclient: the request body cannot be sent twice")

// mark records where each file part's body was before it was read, so that
// [Multipart.rewind] can put it back exactly there.
//
// Not to the start: a caller who seeks into a file and uploads the tail of it
// means the tail, and a retry that helpfully sent the whole thing would be
// sending bytes nobody asked for. A body that cannot seek is marked -1 — it
// works the first time and refuses a second, which is all of [ErrCannotRetry].
//
// It is called before the first send, so an unseekable body is not a failure
// here: most uploads are never retried.
func (m *Multipart) mark() []int64 {
	if m == nil {
		return nil
	}

	marks := make([]int64, len(m.Files))
	for i, f := range m.Files {
		marks[i] = -1
		if s, ok := f.Body.(io.Seeker); ok {
			if at, err := s.Seek(0, io.SeekCurrent); err == nil {
				marks[i] = at
			}
		}
	}
	return marks
}

// rewind puts every file part back where [Multipart.mark] found it, so the
// request can be sent again.
//
// A body that cannot seek is refused rather than buffered, which is the trade
// [ErrCannotRetry] exists to state. JSON is free: it is marshalled afresh on
// every send.
func (m *Multipart) rewind(marks []int64) error {
	if m == nil {
		return nil
	}
	if len(marks) != len(m.Files) {
		return fmt.Errorf("%w: the form changed between sends", ErrCannotRetry)
	}

	for i, f := range m.Files {
		if marks[i] < 0 {
			return fmt.Errorf("%w: the part %q is a reader that cannot seek",
				ErrCannotRetry, f.Field)
		}
		if _, err := f.Body.(io.Seeker).Seek(marks[i], io.SeekStart); err != nil {
			return fmt.Errorf("%w: rewinding the part %q: %w", ErrCannotRetry, f.Field, err)
		}
	}
	return nil
}

// stream writes the form to a pipe and returns the reader half.
//
// Nothing is buffered: the bytes go from the caller's reader to the connection,
// so a file larger than memory is an ordinary upload rather than an
// out-of-memory. The cost is that the length is unknown until it has all been
// written, so the request is sent chunked — see [Op.Multipart].
func (m *Multipart) stream() (io.ReadCloser, string) {
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		// CloseWithError on the write half is what makes a failure halfway
		// through a file arrive at the transport as that failure, rather than as
		// a body that simply stopped.
		pw.CloseWithError(m.write(w))
	}()
	return pr, w.FormDataContentType()
}

// write emits every part in order, and closes the form.
func (m *Multipart) write(w *multipart.Writer) error {
	if m.JSON != nil {
		part, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="json"`},
			"Content-Type":        {"application/json"},
		})
		if err != nil {
			return err
		}
		if err := json.NewEncoder(part).Encode(m.JSON); err != nil {
			return fmt.Errorf("rigclient: encoding the json part: %w", err)
		}
	}

	for _, f := range m.Files {
		if f.Field == "" {
			return errors.New("rigclient: a file part with no field name")
		}
		if f.Body == nil {
			return fmt.Errorf("rigclient: the part %q has no body", f.Field)
		}

		// Not CreateFormFile: it hardcodes application/octet-stream and gives no
		// way to send what the caller says the bytes are.
		header := textproto.MIMEHeader{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
				safeName(f.Field), safeName(f.Name))},
		}
		if f.ContentType != "" {
			header.Set("Content-Type", safeName(f.ContentType))
		}

		part, err := w.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, f.Body); err != nil {
			return fmt.Errorf("rigclient: sending the part %q: %w", f.Field, err)
		}
	}
	return w.Close()
}

// headerSafe is what a name becomes inside a part's Content-Disposition.
//
// The quote escaping is what mime/multipart does, and has to be done here
// because the header is built by hand rather than by CreateFormFile. The
// carriage return and newline are not: stdlib leaves those alone, and a name
// carrying them would end the header and start whatever came next. An
// application proxying an upload passes through a name somebody else chose, so
// this is the one place that assumption gets made for it.
var headerSafe = strings.NewReplacer(
	"\\", "\\\\",
	`"`, "\\\"",
	"\r", "",
	"\n", "",
)

func safeName(s string) string { return headerSafe.Replace(s) }
