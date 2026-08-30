// Package files is the half of an upload that is the same in every project.
//
// A file in rig is two things that cannot be written together: a row in
// rig_file, and an object in a [blob.Store]. Everything here is about keeping
// those two in step — which order they are written in, which one leads when they
// disagree, and what a sweeper is allowed to remove. That is the part every
// project gets subtly wrong, and it is the part rig takes. What a file *means* —
// derivatives, retention policy, whether a photo needs a thumbnail — is where
// products diverge hardest and stays yours.
//
// The generated server calls [Service]. The generated client never sees this
// module at all: a program that calls a rig application depends on rigclient,
// and rigclient knows about multipart bodies and nothing about buckets.
//
// # Which way round the writes go
//
// The row leads, always. A file row is inserted with a null uploaded_at and
// committed alone, the bytes are streamed to the store, and then one transaction
// finalizes the row and writes the owner. Every read filters
// `uploaded_at IS NOT NULL`, so a row with no bytes behind it is invisible and
// reapable with one query. Bytes with no row would need a bucket scan to find.
//
// The single transaction at the end is what keeps the sweeper's two rules
// sufficient. Finalize the file and write the owner separately and a crash
// between them leaves a file that is uploaded and referenced by nothing — which
// neither rule catches, and which would force exactly the unreferenced-file
// reaper this design refuses to have.
package files

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
)

// DefaultMaxBytes is the largest single upload a project accepts unless it says
// otherwise. It is a hard per-file cap and not a quota: rig does not do storage
// quotas, and saying so is better than implying one.
const DefaultMaxBytes int64 = 25 << 20

// Defaults for the two intervals the sweeper reads.
const (
	// DefaultAbandonedAfter is how long a row with no bytes behind it gets the
	// benefit of the doubt before it is treated as the remains of a request that
	// died.
	DefaultAbandonedAfter = 24 * time.Hour
	// DefaultRestoreWindow is how long a deleted file stays restorable, and
	// therefore how long its bytes outlive the delete.
	DefaultRestoreWindow = 30 * 24 * time.Hour
)

// DefaultInlineTypes are served without an attachment disposition.
//
// The list is short on purpose, and text/html must never join it. A file served
// inline from the API origin runs there, and the URL is on a row a client syncs,
// so it will end up in an <img> or an <a> without anybody thinking about it.
// Everything not named here downloads.
var DefaultInlineTypes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif",
	"application/pdf",
}

// File is one row of rig_file, as the server holds it.
//
// It is not the shape a client sees. StorageKey, Checksum and DeclaredType stay
// here: the storage key is what a signed URL is built from, and putting it
// anywhere a client can reach is the same class of mistake as syncing a password
// hash.
type File struct {
	ID       uuid.UUID
	TenantID uuid.UUID

	StorageKey string
	// URL is where the file is served from — the download route, with the
	// identifier and the name in it. Stable and unsigned, so holding it grants
	// nothing and the endpoint behind it still authorizes.
	URL string

	Name string
	// ContentType is what the bytes were sniffed to be, which is what a download
	// announces.
	ContentType string
	// DeclaredType is what the client claimed, kept beside the sniffed type
	// rather than instead of it.
	DeclaredType string

	Size     int64
	Checksum string

	CreatedAt  time.Time
	UploadedAt *time.Time
	DeletedAt  *time.Time
}

// Upload is a file on its way in.
//
// Body is read once, streamed straight to the store, and never buffered: an
// upload larger than memory is an ordinary upload. Whoever produced it closes
// it — for the generated handler that is the request body, which net/http
// already owns.
type Upload struct {
	// Name is what the file is called. It goes on the row and into the download
	// path, and it never becomes the storage key: see [blob.Key].
	Name string
	// DeclaredType is the type the client claimed. It is recorded and then
	// largely ignored — the sniffed type is the one that is stored and served.
	DeclaredType string
	Body         io.Reader
}

// Content is a file on its way out: the row, and the bytes still in the store.
//
// Body is the store's reader and is the caller's to close. Nothing reads ahead,
// which is what lets a file larger than memory go straight to a response — and
// it is why it seeks, because http.ServeContent cannot answer a range request
// without that.
type Content struct {
	File *File
	Body io.ReadSeekCloser
}

// Owner names the row a file hangs off: the table, the row, and the column.
//
// The table and column are written by a generator from the document, never by a
// request, which is what makes it safe for [Service] to build a statement out of
// them.
type Owner struct {
	Table string
	// IDColumn is the owning table's primary key, normally "id".
	IDColumn string
	// TenantColumn scopes the update. Empty for a table with no tenant column,
	// which is not a shape rig generates but is one a test can build.
	TenantColumn string
	// FileColumn is the `<role>_file_id` column the identifier is written to.
	FileColumn string

	ID uuid.UUID

	// Forget withdraws whatever is being held of this row, and it exists because
	// this is the one place rig writes an application's table from outside that
	// table's generated repository.
	//
	// A table that holds its rows between requests is invalidated by the writes
	// its repository makes, and the statement below is not one of them: attaching
	// a file changes the row's `<role>_file_id` and every replica holding it would
	// go on saying there is no file there — which the download endpoint answers as
	// a 404, for a file that was just uploaded successfully.
	//
	// Generated code fills this in for a table that asked to be held, and it is
	// called inside the transaction that writes the column, so the withdrawal is
	// atomic with the change and discarded by a rollback. Nil is the ordinary
	// case and means nothing is held.
	Forget func(ctx context.Context) error
}

// Config is everything a [Service] needs that is not a request.
//
// It is resolved from the `files:` block in rig.yaml and written into
// files.gen.go by server-go, so a byte cap or a sweep interval is a line in a
// file the generated documentation can quote rather than a literal in a main
// function nobody diffs.
type Config struct {
	// Store is where the bytes go.
	Store blob.Store
	// DB is the pool the owning tables are in. The same one, deliberately: the
	// transaction that finalizes a file and writes its owner has to be one
	// transaction, and it cannot be if the two live in different pools.
	DB DB

	// MaxBytes caps one upload. Zero means [DefaultMaxBytes].
	MaxBytes int64
	// InlineTypes are the sniffed types served without an attachment
	// disposition. Nil means [DefaultInlineTypes]; an explicitly empty slice
	// means everything downloads.
	InlineTypes []string

	// AbandonedAfter and RestoreWindow are the sweeper's two rules. Zero means
	// the defaults.
	AbandonedAfter time.Duration
	RestoreWindow  time.Duration

	// Now is the clock, so a test can move it. Nil means time.Now.
	Now func() time.Time

	// Logger is where [Service.Sweep] says what it reaped.
	//
	// Nil is not silence: it is [log/slog.Default], the reading every other
	// Logger in rig gives it. [SweepReport] exists for "the log line the task
	// writes" and the generated task discards it, so what a project's
	// housekeeping actually did has never been written anywhere.
	//
	// The line is DEBUG. A sweep that failed is not logged here at all: the
	// error is returned, and whatever ran the task reports it once.
	Logger *slog.Logger
}

func (c Config) maxBytes() int64 {
	if c.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return c.MaxBytes
}

func (c Config) inlineTypes() []string {
	if c.InlineTypes == nil {
		return DefaultInlineTypes
	}
	return c.InlineTypes
}

func (c Config) abandonedAfter() time.Duration {
	if c.AbandonedAfter <= 0 {
		return DefaultAbandonedAfter
	}
	return c.AbandonedAfter
}

func (c Config) restoreWindow() time.Duration {
	if c.RestoreWindow <= 0 {
		return DefaultRestoreWindow
	}
	return c.RestoreWindow
}

// log is where a sweep says what it did, and [log/slog.Default] when the
// configuration named nowhere.
func (c Config) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// ErrNotFound is what a lookup answers for a file that is not there, has not
// finished uploading, or belongs to another tenant.
//
// One error for all three, deliberately. Distinguishing them would tell a caller
// that a file exists and is somebody else's, which is exactly what a tenant
// boundary is for.
var ErrNotFound = errors.New("files: no such file")
