// Package blob is where the bytes live.
//
// A rig_file row is metadata: who owns it, what it is called, how big it is,
// whether it has been uploaded yet. The object itself is somewhere else, and
// this is the seam between them. Two implementations ship — [Memory], for tests
// and `go run`, and S3 in a module of its own so that a project which never
// calls it does not carry the SDK.
//
// [Signer] and [Marker] are separate interfaces on purpose. A backend that
// cannot mint a URL, or has nowhere to record that an object is deleted, simply
// does not have the method — which a caller can ask about — rather than having
// one that always returns an error, which a caller finds out about in
// production.
package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is what a Get or a Stat answers for a key that is not there.
//
// It is a sentinel rather than a typed error because the only thing a caller
// ever needs to know is which of the two cases it has, and errors.Is is how the
// rest of the standard library asks that question.
var ErrNotFound = errors.New("blob: no object with that key")

// Store is the backend the bytes are kept in.
//
// Every method takes a key rather than anything richer, because a store knows
// nothing about tenants, rows or permissions. Everything that decides who may
// reach an object happens before a call gets here.
type Store interface {
	// Put writes the object and reports how many bytes it took and what they
	// hash to.
	//
	// The checksum comes back from the write rather than being handed to it,
	// because it cannot be known until the last byte has gone past — which is
	// also why a rig_file row is finalized after the upload rather than before.
	Put(ctx context.Context, key string, r io.Reader, opt PutOptions) (Info, error)

	// Get opens the object for reading.
	//
	// The reader is a ReadSeekCloser so a download can answer a range request:
	// http.ServeContent needs to seek, and without it a video cannot be scrubbed
	// and a resumed download starts from the beginning.
	Get(ctx context.Context, key string) (io.ReadSeekCloser, error)

	// Stat reports what is known about the object without opening it.
	Stat(ctx context.Context, key string) (Info, error)

	// Delete removes the object, and says nothing about a key that was already
	// gone.
	//
	// Idempotence is what lets the sweeper be a simple loop: it runs over
	// whatever the table says is expired, and a second pass over the same row
	// after a crash is not an error to handle.
	Delete(ctx context.Context, key string) error
}

// PutOptions is what the store is told about an object it is being handed.
type PutOptions struct {
	// ContentType is stored with the object so a backend that serves bytes
	// directly labels them correctly. rig never trusts it on the way back —
	// the type a download announces is the one that was sniffed at upload and
	// recorded in the row.
	ContentType string
}

// Info is what a store knows about an object.
type Info struct {
	Size int64
	// Checksum is the hex-encoded SHA-256 of the content.
	//
	// Not an ETag: an ETag is whatever the backend felt like, and S3 makes it
	// something other than the MD5 as soon as an upload is multipart. A checksum
	// rig computes as the bytes go past means the same thing everywhere.
	Checksum string
	ModTime  time.Time
}

// Signer is the optional half of a store that can mint a URL for a client to
// upload to directly.
//
// There is deliberately no signed *download*. A signed URL bypasses the
// endpoint, and the endpoint is where the permission check lives — so downloads
// always flow through rig, and the honest cost is bandwidth through the
// application.
type Signer interface {
	SignUpload(ctx context.Context, key string, expires time.Duration) (string, error)
}

// State is what a [Marker] records about an object.
type State string

const (
	// StateLive is an object whose row is not deleted.
	StateLive State = "live"
	// StateDeleted is an object whose row has been soft-deleted and which the
	// sweeper will remove once the restore window closes.
	StateDeleted State = "deleted"
)

// Marker is the optional half of a store that can record a deletion on the
// object itself, rather than only in the database.
//
// Without it, the bucket cannot answer "what here is deleted?" — so a bucket
// audit, or a bucket restored from one snapshot beside a database restored from
// another, has no way to reconcile. And when somebody asks whether a file was
// deleted, the object is usually the thing they mean; the row is rig's
// bookkeeping.
//
// The row leads and the mark follows: set deleted_at, commit, then mark, and
// treat the mark as best-effort. A failed mark leaves a deleted row beside an
// unmarked object, which is the safe direction — the sweeper still knows from
// the row and re-marks anything out of step. Marking first would produce an
// object tagged deleted that the database says is live, and nothing reconciles
// that back.
type Marker interface {
	Mark(ctx context.Context, key string, state State, at time.Time) error
}

// Key is the storage key for a file, derived from its identifier and nothing
// else.
//
// The name a client uploaded never reaches the store. It goes in the row, where
// it is data; letting it into the key makes it a path, and "../../etc/passwd"
// or an absolute path or a name that differs only by case are all then somebody
// else's problem to get right. A uuid has none of those questions.
//
// The two-level prefix is for the backends that shard by key prefix, and costs
// nothing on the ones that do not.
func Key(id uuid.UUID) string {
	s := id.String()
	return "files/" + s[0:2] + "/" + s[2:4] + "/" + s
}
