package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// Service is what a generated service layer calls.
//
// Every method takes the tenant explicitly rather than reading it off the
// context, because the caller has already resolved the owning row through a
// repository that scoped the read — and passing the same tenant through means
// the two cannot come apart in a way only a test with two tenants would notice.
type Service struct {
	cfg   Config
	store store
}

// New builds the service.
func New(cfg Config) *Service {
	return &Service{cfg: cfg, store: store{db: cfg.DB}}
}

// MaxBytes is the cap this service applies to one upload, so a handler can set
// up its own limit reader from the same number.
func (s *Service) MaxBytes() int64 { return s.cfg.maxBytes() }

// InlineTypes are the sniffed types this service serves without an attachment
// disposition.
func (s *Service) InlineTypes() []string { return s.cfg.inlineTypes() }

// AttachRequest is one upload: what arrived, where it goes, and what its URL
// will be.
type AttachRequest struct {
	TenantID uuid.UUID
	Upload   Upload

	// Owner is the row the file hangs off. Its identifier is zero for a file
	// being uploaded as part of the create that will hold it — see
	// [Service.Prepare].
	Owner Owner

	// URL builds the download path once the file has an identifier and a name.
	// The endpoint's own route is what it renders, which is why a generator
	// supplies it rather than this package guessing.
	URL func(fileID uuid.UUID, name string) string
}

// Attach stores an upload and points an existing row at it.
//
// Three steps, in this order and no other. The row is inserted with no bytes
// behind it and committed alone; the bytes are streamed to the store; and then
// one transaction finalizes the row and writes the owning column. A failure
// anywhere leaves at worst a pending row, which is invisible to every read and
// which the sweeper reaps.
//
// Replacing a file does not delete the one it replaces. A table that keeps its
// previous versions still points at the old file from every one of them, and
// deleting it on replace would corrupt exactly the history the snapshot exists
// to keep.
func (s *Service) Attach(ctx context.Context, req AttachRequest) (*File, error) {
	f, err := s.upload(ctx, req)
	if err != nil {
		return nil, err
	}

	err = dbx.InTx(ctx, s.cfg.DB, func(ctx context.Context, _ dbx.Conn) error {
		if err := s.store.finalize(ctx, f.ID, f.URL, f.Size, f.Checksum, f.ContentType, *f.UploadedAt); err != nil {
			return err
		}
		return s.store.attach(ctx, req.Owner, req.TenantID, &f.ID)
	})
	if err != nil {
		return nil, notFound(err)
	}
	return f, nil
}

// Pending is a file whose bytes have landed and whose row is not final: the
// second half of a create that carried its file with it.
//
// It exists because a create is one transaction and an upload cannot be in it.
// The bytes go first, outside; then the row and its files are committed
// together by [Service.Commit], which is what makes a not-null file column
// expressible at all.
type Pending struct {
	File *File
	// Part is the form part this came from, so a generated create can put each
	// identifier on the right column.
	Part string
}

// Prepare stores an upload that a create is carrying, and hands back the file
// without attaching it to anything.
//
// The row is not visible yet: uploaded_at is still null, so nothing reads it and
// the sweeper will take it if the create never lands.
func (s *Service) Prepare(ctx context.Context, part string, req AttachRequest) (*Pending, error) {
	f, err := s.upload(ctx, req)
	if err != nil {
		return nil, err
	}
	return &Pending{File: f, Part: part}, nil
}

// Commit runs a write and finalizes the files it carried, in one transaction.
//
// One transaction is the whole point. Commit the row and the files separately
// and a crash between them leaves files that are uploaded and referenced by
// nothing — a state neither sweeper rule catches, and the reason this design
// refuses to have an unreferenced-file reaper at all. Fail anywhere and the
// files stay pending, which is invisible and reapable.
//
// write runs first because the URL cannot be built until the owning row has an
// identifier, which is also why url is a function rather than a field: for a
// create, the row it points at does not exist when the bytes are stored.
func (s *Service) Commit(
	ctx context.Context,
	pending []*Pending,
	write func(ctx context.Context) error,
	url func(p *Pending) string,
) error {
	return dbx.InTx(ctx, s.cfg.DB, func(ctx context.Context, _ dbx.Conn) error {
		if err := write(ctx); err != nil {
			return err
		}
		for _, p := range pending {
			f := p.File
			if url != nil {
				f.URL = url(p)
			}
			if err := s.store.finalize(ctx, f.ID, f.URL, f.Size, f.Checksum, f.ContentType, *f.UploadedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// upload is the part both Attach and Prepare share: insert, stream, and report
// what the store said.
func (s *Service) upload(ctx context.Context, req AttachRequest) (*File, error) {
	if s.cfg.Store == nil {
		return nil, rigerr.Internal(errNoStore, "this project accepts no uploads")
	}
	if req.Upload.Body == nil {
		return nil, rigerr.Invalid("no file was sent")
	}

	now := s.cfg.now()
	f := &File{
		ID:           uuid.New(),
		TenantID:     req.TenantID,
		Name:         cleanName(req.Upload.Name),
		DeclaredType: req.Upload.DeclaredType,
		CreatedAt:    now,
	}
	f.StorageKey = blob.Key(f.ID)

	// The type is sniffed rather than believed. `evil.html` uploaded as
	// text/html and served back from the API origin is stored XSS, and it
	// matters more here than usual because the URL ends up on a row a client
	// syncs and then in an <img> or an <a> without anybody thinking about it.
	body, sniffed := sniff(req.Upload.Body)
	f.ContentType = sniffed

	// Before the bytes, so a failed upload leaves a row the sweeper knows about
	// rather than an object nothing points at.
	if err := s.store.begin(ctx, f); err != nil {
		return nil, err
	}

	limited := &limitedReader{r: body, left: s.cfg.maxBytes()}
	info, err := s.cfg.Store.Put(ctx, f.StorageKey, limited, blob.PutOptions{ContentType: sniffed})
	if err != nil {
		if limited.exceeded {
			return nil, rigerr.TooLarge("this endpoint accepts files up to %d bytes", s.cfg.maxBytes())
		}
		return nil, rigerr.Internal(err, "the file could not be stored")
	}
	if limited.exceeded {
		return nil, rigerr.TooLarge("this endpoint accepts files up to %d bytes", s.cfg.maxBytes())
	}

	f.Size = info.Size
	f.Checksum = info.Checksum
	f.UploadedAt = &now
	if req.URL != nil {
		f.URL = req.URL(f.ID, f.Name)
	}
	return f, nil
}

// Open reads a file back.
//
// The name is checked against the stored one and a mismatch is a not-found: the
// identifier is the lookup, and the name in the path is for the browser's save
// dialog and for cache busting. It never goes near the storage key.
//
// A deleted file inside the restore window still opens, because a restore has to
// hand back a row pointing at something and the bytes outlive the delete for
// exactly that reason.
func (s *Service) Open(ctx context.Context, tenantID, fileID uuid.UUID, name string) (*Content, error) {
	if s.cfg.Store == nil {
		return nil, rigerr.Internal(errNoStore, "this project accepts no uploads")
	}

	f, err := s.store.getAny(ctx, tenantID, fileID)
	if err != nil {
		return nil, notFound(err)
	}
	if name != "" && name != f.Name {
		return nil, rigerr.NotFound("no such file")
	}

	body, err := s.cfg.Store.Get(ctx, f.StorageKey)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, rigerr.NotFound("no such file")
		}
		return nil, rigerr.Internal(err, "the file could not be read")
	}
	return &Content{File: f, Body: body}, nil
}

// Get reads a file's metadata without opening it.
func (s *Service) Get(ctx context.Context, tenantID, fileID uuid.UUID) (*File, error) {
	f, err := s.store.get(ctx, tenantID, fileID)
	if err != nil {
		return nil, notFound(err)
	}
	return f, nil
}

// Detach clears a file column and retires the file it pointed at.
//
// The row leads and the mark follows: the deletion is committed first, then the
// object is marked, and the mark is best-effort. A failed mark leaves a deleted
// row beside an unmarked object, which is the safe direction — the sweeper still
// knows from the row and re-marks anything out of step on its next pass. Marking
// first would produce an object tagged deleted that the database says is live,
// and nothing reconciles that back.
func (s *Service) Detach(ctx context.Context, tenantID uuid.UUID, o Owner, fileID uuid.UUID) error {
	at := s.cfg.now()

	f, err := s.store.get(ctx, tenantID, fileID)
	if err != nil {
		return notFound(err)
	}

	err = dbx.InTx(ctx, s.cfg.DB, func(ctx context.Context, _ dbx.Conn) error {
		if err := s.store.attach(ctx, o, tenantID, nil); err != nil {
			return err
		}
		return s.store.softDelete(ctx, tenantID, fileID, at)
	})
	if err != nil {
		return notFound(err)
	}

	s.mark(ctx, f.StorageKey, blob.StateDeleted, at)
	return nil
}

// Restore brings a retired file back, inside the window.
//
// Past the window the bytes are gone, and answering anything but a refusal would
// hand back a row pointing at nothing.
func (s *Service) Restore(ctx context.Context, tenantID, fileID uuid.UUID) (*File, error) {
	f, err := s.store.getAny(ctx, tenantID, fileID)
	if err != nil {
		return nil, notFound(err)
	}
	if f.DeletedAt == nil {
		return f, nil
	}
	if s.cfg.now().Sub(*f.DeletedAt) > s.cfg.restoreWindow() {
		return nil, rigerr.Conflict("this file was deleted more than %s ago and its bytes are gone",
			s.cfg.restoreWindow())
	}

	if err := s.store.restore(ctx, tenantID, fileID); err != nil {
		return nil, err
	}
	s.mark(ctx, f.StorageKey, blob.StateLive, s.cfg.now())

	f.DeletedAt = nil
	return f, nil
}

// mark records the state on the object itself, when the backend has somewhere to
// put it.
//
// Best-effort and deliberately silent about failure: the mark is a projection of
// the row and is always re-derivable from it, and a delete that succeeded should
// not report an error because a bucket tag did not.
func (s *Service) mark(ctx context.Context, key string, state blob.State, at time.Time) {
	m, ok := s.cfg.Store.(blob.Marker)
	if !ok {
		return
	}
	_ = m.Mark(ctx, key, state, at)
}

// notFound turns this package's sentinel into the failure a handler answers
// with, and leaves everything else alone.
func notFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return rigerr.NotFound("no such file")
	}
	return err
}

// cleanName strips a supplied name back to something safe to put in a header and
// in a URL path segment.
//
// It never becomes the storage key — see [blob.Key] — so this is not the last
// line of defence, and it should not have to be. What it does stop is a name
// that would break Content-Disposition or add a segment to the download path.
func cleanName(name string) string {
	if i := lastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == '/' || r == '\\' {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "file"
	}
	return string(out)
}

func lastIndexAny(s, chars string) int {
	best := -1
	for i, r := range s {
		for _, c := range chars {
			if r == c && i > best {
				best = i
			}
		}
	}
	return best
}

// sniff reads the leading bytes, decides what they are, and hands back a reader
// that still has them.
//
// http.DetectContentType wants up to 512 bytes and the store wants all of them,
// so the peeked prefix is stitched back on rather than read twice — the body is
// a network stream and there is no second pass to be had.
func sniff(r io.Reader) (io.Reader, string) {
	head := make([]byte, 512)
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	return io.MultiReader(bytes.NewReader(head), r), http.DetectContentType(head)
}

// limitedReader refuses rather than truncating.
//
// io.LimitReader reports EOF at the limit, which to a store is a complete
// object one byte short of the truth. The difference between the two is silent
// data loss, so this reports the failure instead.
type limitedReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	// One byte past the allowance is read on purpose: a file exactly at the cap
	// is accepted, and it is the byte after it that proves there was one.
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		l.exceeded = true
		return 0, ErrTooLarge
	}
	return n, err
}

// ErrTooLarge is what a store's Put sees when an upload runs past the cap. A
// caller gets [rigerr.CodeTooLarge] rather than this.
var ErrTooLarge = errors.New("files: the upload is larger than this endpoint accepts")
