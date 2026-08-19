// Package filehttp is the HTTP shape of an upload and a download.
//
// It is here rather than in generated code because it is four hundred lines that
// would otherwise be regenerated identically into every project — and because
// every one of the decisions in it has an obvious wrong answer that a generator
// golden would never notice. The type a download announces, whether a form
// spills to disk, whether a cap truncates or refuses: each is one line, and each
// is the difference between a feature and an incident.
//
// The generated handler calls [ReadForm] on the way in and [Serve] on the way
// out. Nothing here decides who may do anything; that has already happened by
// the time a call gets in.
package filehttp

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/simonjanss/rig/files"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// JSONPart is the part a multipart create carries its row in.
//
// One fixed name rather than one per resource: the form already names its file
// parts after the columns they land on, and a second naming scheme for the body
// would be a thing to look up for no gain.
const JSONPart = "json"

// Form is a multipart request, read one part at a time.
//
// The parts arrive in the order the client wrote them and are not buffered, so
// the caller has to consume each one before asking for the next. That is the
// whole reason this is an iterator rather than a map: a map of parts is a map of
// files in memory.
type Form struct {
	reader *multipart.Reader
	// JSON is the "json" part's bytes, when the form had one. It is read eagerly
	// because it is small by construction and because a create needs it before
	// it can decide what to do with the files.
	JSON []byte
}

// ReadForm starts reading a multipart body.
//
// It uses r.MultipartReader rather than r.ParseMultipartForm, and that is not a
// style preference: ParseMultipartForm spills every part past its memory budget
// into os.TempDir, so an upload of a file larger than memory lands on the
// ephemeral disk of a container that probably does not have room for it — twice,
// once there and once in the store.
//
// maxBytes caps each file part. It is applied by the reader the caller streams
// from, not here, because the cap has to refuse rather than truncate and only
// the thing doing the reading can tell the difference.
func ReadForm(r *http.Request) (*Form, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		if errors.Is(err, http.ErrNotMultipart) {
			return nil, rigerr.UnsupportedMediaType("this endpoint takes a multipart/form-data body")
		}
		return nil, rigerr.BadRequest("the form could not be read: %v", err)
	}
	return &Form{reader: mr}, nil
}

// Part is one part of a form: either the JSON body or a file.
type Part struct {
	// Name is the part's name on the wire.
	Name string
	// Filename is what the client called the file, or empty for a part that is
	// not a file.
	Filename string
	// ContentType is what the client claimed. It is recorded and then largely
	// ignored: the type that is stored and served is the one sniffed from the
	// bytes.
	ContentType string
	// Body is the part's content, valid until Next is called again.
	Body io.Reader
}

// Next reads the next part, or reports io.EOF.
//
// The returned part's body is only valid until the following call: this is a
// stream, and the bytes are gone once the reader has moved on.
func (f *Form) Next() (*Part, error) {
	p, err := f.reader.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, rigerr.BadRequest("the form could not be read: %v", err)
	}
	return &Part{
		Name:        p.FormName(),
		Filename:    p.FileName(),
		ContentType: p.Header.Get("Content-Type"),
		Body:        p,
	}, nil
}

// Upload turns a part into what [files.Service] takes.
func (p *Part) Upload() files.Upload {
	return files.Upload{Name: p.Filename, DeclaredType: p.ContentType, Body: p.Body}
}

// OnePart reads a form that carries exactly one file, under the given name.
//
// It is the upload route's whole body handling. The returned upload's Body is
// the part itself and is streamed, so the caller has to consume it before doing
// anything else with the request.
//
// A second part, or a first one under another name, is refused rather than
// ignored — see [ErrUnknownPart].
func OnePart(r *http.Request, name string) (files.Upload, error) {
	form, err := ReadForm(r)
	if err != nil {
		return files.Upload{}, err
	}

	part, err := form.Next()
	if errors.Is(err, io.EOF) {
		return files.Upload{}, ErrMissingPart(name)
	}
	if err != nil {
		return files.Upload{}, err
	}
	if part.Name != name {
		return files.Upload{}, ErrUnknownPart(part.Name)
	}
	return part.Upload(), nil
}

// ErrUnknownPart is what a form carrying a part nobody claimed produces.
//
// Refused rather than skipped, for the reason DisallowUnknownFields refuses an
// unknown key: a client that misspelled a part name has uploaded a file into
// nowhere and would otherwise get a 201 saying it worked.
func ErrUnknownPart(name string) error {
	return rigerr.Invalid("the form carries a part named %q, which this endpoint does not accept", name)
}

// ErrMissingPart is what a form missing a required file part produces.
//
// A field error and not a 500: a not-null file column with no part is the same
// class of mistake as a missing required field, and it should read like one.
func ErrMissingPart(name string) error {
	return rigerr.Invalid("the form has no part named %q, which this endpoint requires", name)
}

// IsMultipart reports whether a request carries a form.
//
// It is how a create decides which of its two bodies it was sent. A request with
// no multipart content type takes the JSON path it has always taken, byte for
// byte.
func IsMultipart(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	return err == nil && strings.HasPrefix(mediaType, "multipart/")
}

// Serve writes a file's bytes back.
//
// http.ServeContent does the work, which is what gets range and conditional
// requests answered for free — a resumed download that does not start over, and
// a media element that can seek. Both are the kind of thing discovered in
// production rather than in a test, and neither is worth reimplementing.
//
// The caller closes c.Body.
func Serve(w http.ResponseWriter, r *http.Request, c *files.Content, inlineTypes []string) {
	// Always. A browser that sniffs its own type out of bytes the server has
	// already decided about is the whole attack this is for.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", c.File.ContentType)
	w.Header().Set("Content-Disposition", disposition(c.File.Name, c.File.ContentType, inlineTypes))
	if c.File.Checksum != "" {
		w.Header().Set("ETag", `"`+c.File.Checksum+`"`)
	}

	modtime := c.File.CreatedAt
	if c.File.UploadedAt != nil {
		modtime = *c.File.UploadedAt
	}
	// The name is passed empty on purpose. ServeContent would otherwise guess a
	// content type from its extension, and the whole point is that the type is
	// the one sniffed from the bytes at upload.
	http.ServeContent(w, r, "", modtime.UTC(), c.Body)
}

// disposition decides whether a file renders in the browser or downloads.
//
// Attachment unless the sniffed type is on a short allowlist. A file served
// inline from the API origin runs there, and the URL is on a row a client syncs,
// so it will end up in an <img> or an <a> without anybody thinking about it.
// text/html is the one that must never be on the list.
func disposition(name, contentType string, inlineTypes []string) string {
	kind := "attachment"
	if slices.Contains(inlineTypes, baseType(contentType)) {
		kind = "inline"
	}
	return kind + "; " + filenameParams(name)
}

func baseType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.TrimSpace(contentType)
}

// filenameParams renders the name for Content-Disposition, in both forms when it
// needs the second.
//
// The plain form is ASCII only, so a name that is not gets the RFC 5987 form
// beside it — beside rather than instead, because the plain parameter is what an
// old client reads and dropping it would leave one with no name at all.
func filenameParams(name string) string {
	ascii := make([]rune, 0, len(name))
	simple := true
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			simple = false
			ascii = append(ascii, '_')
			continue
		}
		ascii = append(ascii, r)
	}

	out := `filename="` + string(ascii) + `"`
	if !simple {
		out += `; filename*=UTF-8''` + url.PathEscape(name)
	}
	return out
}

// Deadline extends this one request's clock without touching the server's.
//
// ReadTimeout and WriteTimeout are set once on the one http.Server, so raising
// them for a two-hundred-megabyte upload weakens every other route in the
// application. This is the per-request form, which is why there is no
// UploadTimeout field on serve.Config and should not be one.
//
// A server that does not support the control simply keeps its own deadlines,
// which is the right failure: the upload is bounded by something rather than by
// nothing.
func Deadline(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	now := time.Now()
	_ = rc.SetReadDeadline(now.Add(d))
	_ = rc.SetWriteDeadline(now.Add(d))
}

// DefaultDeadline is how long one transfer gets. Generous, because it is bounded
// by the size cap on the way in and by the client on the way out, and a slow
// mobile connection uploading twenty megabytes is not a failure.
const DefaultDeadline = 30 * time.Minute
