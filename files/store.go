package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/dbx"
)

// DB is the pool the file rows and their owners live in.
//
// It is the two dbx interfaces together rather than *pgxpool.Pool, so that a
// test can hand this a transaction and so the module never imports pgxpool.
type DB interface {
	dbx.Conn
	dbx.Beginner
}

// Table is the managed table every uploaded file has a row in. Spelled once,
// here, because it is the only name in this package a migration also has to
// know.
const Table = "rig_file"

// columns is what every read of a file row selects, in one place so that the
// scan below cannot drift from it.
const columns = `id, tenant_id, storage_key, url, file_name, content_type,
	declared_content_type, size_bytes, checksum, created_at, uploaded_at, deleted_at`

// store is the rig_file half of the module: SQL, and nothing that decides
// anything.
//
// It is hand-written rather than generated, for the reason the foundation
// already gives: rig_file has no generated CRUD, because a client that could
// POST a row with an arbitrary storage key and no bytes has found a way around
// every rule the upload endpoint enforces. There is no flat endpoint to generate
// against under this design anyway.
type store struct{ db DB }

// conn is the transaction on the context, or the pool.
//
// Every statement goes through it, which is what lets [Service.Attach] finalize
// a file and write its owner in one transaction opened somewhere above.
func (s store) conn(ctx context.Context) dbx.Conn {
	if tx, ok := dbx.Tx(ctx); ok {
		return tx
	}
	return s.db
}

// begin inserts a file row with no bytes behind it yet and commits it alone.
//
// uploaded_at is null, so nothing can read this row and the sweeper's first rule
// can reap it. That is the safe direction and the only one: bytes with no row
// would need a bucket scan to find, and a row with no bytes needs one query.
func (s store) begin(ctx context.Context, f *File) error {
	const q = `INSERT INTO ` + Table + `
		(id, tenant_id, storage_key, file_name, content_type, declared_content_type,
		 size_bytes, created_at, created_by_account_id, created_by_api_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, 0, $7, $8, $9)`

	_, err := s.conn(ctx).Exec(ctx, q,
		f.ID, f.TenantID, f.StorageKey, f.Name, f.ContentType, nullString(f.DeclaredType),
		f.CreatedAt, nil, nil)
	if err != nil {
		return fmt.Errorf("files: begin upload: %w", err)
	}
	return nil
}

// finalize records what the store reported and makes the row visible.
//
// It is the caller's job to run this in the same transaction as the write that
// attaches the file to its owner. Committing them separately leaves a finalized
// file referenced by nothing, which is the one state neither sweeper rule
// catches.
func (s store) finalize(ctx context.Context, id uuid.UUID, url string, size int64, checksum, contentType string, at time.Time) error {
	const q = `UPDATE ` + Table + `
		SET url = $2, size_bytes = $3, checksum = $4, content_type = $5, uploaded_at = $6
		WHERE id = $1 AND uploaded_at IS NULL`

	tag, err := s.conn(ctx).Exec(ctx, q, id, url, size, checksum, contentType, at)
	if err != nil {
		return fmt.Errorf("files: finalize upload: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The row went, or somebody finalized it already. Either way this upload
		// has nowhere to land, and reporting success would hand back a file the
		// next read cannot see.
		return ErrNotFound
	}
	return nil
}

// get reads one uploaded, undeleted file.
func (s store) get(ctx context.Context, tenantID, id uuid.UUID) (*File, error) {
	const q = `SELECT ` + columns + ` FROM ` + Table + `
		WHERE id = $1 AND tenant_id = $2 AND uploaded_at IS NOT NULL AND deleted_at IS NULL`

	return scanFile(s.conn(ctx).QueryRow(ctx, q, id, tenantID))
}

// getDeleted reads one file whether or not it is deleted, for the download that
// a restore inside the window has to keep working and for the sweeper.
func (s store) getAny(ctx context.Context, tenantID, id uuid.UUID) (*File, error) {
	const q = `SELECT ` + columns + ` FROM ` + Table + `
		WHERE id = $1 AND tenant_id = $2 AND uploaded_at IS NOT NULL`

	return scanFile(s.conn(ctx).QueryRow(ctx, q, id, tenantID))
}

// softDelete retires a file. The bytes stay until the sweeper reaches them,
// because a restore inside the window has to hand back a row pointing at
// something.
func (s store) softDelete(ctx context.Context, tenantID, id uuid.UUID, at time.Time) error {
	const q = `UPDATE ` + Table + ` SET deleted_at = $3
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`

	tag, err := s.conn(ctx).Exec(ctx, q, id, tenantID, at)
	if err != nil {
		return fmt.Errorf("files: delete file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already deleted, or never there. Deleting something twice is not a
		// failure: it is the state the caller asked for, and answering otherwise
		// would make a retry of a lost response look like an error.
		return nil
	}
	return nil
}

// restore clears the deletion stamp.
func (s store) restore(ctx context.Context, tenantID, id uuid.UUID) error {
	const q = `UPDATE ` + Table + ` SET deleted_at = NULL
		WHERE id = $1 AND tenant_id = $2`

	if _, err := s.conn(ctx).Exec(ctx, q, id, tenantID); err != nil {
		return fmt.Errorf("files: restore file: %w", err)
	}
	return nil
}

// attach writes the file's identifier onto the row that owns it.
//
// The statement is built from the owner's table and column names, which come
// from the document and are compile-time constants in generated code. Nothing a
// request carries reaches the string; the identifiers are parameters.
func (s store) attach(ctx context.Context, o Owner, tenantID uuid.UUID, fileID *uuid.UUID) error {
	q := `UPDATE ` + quoteIdent(o.Table) + ` SET ` + quoteIdent(o.FileColumn) + ` = $1
		WHERE ` + quoteIdent(o.IDColumn) + ` = $2`
	args := []any{fileID, o.ID}
	if o.TenantColumn != "" {
		q += ` AND ` + quoteIdent(o.TenantColumn) + ` = $3`
		args = append(args, tenantID)
	}

	tag, err := s.conn(ctx).Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("files: attach to %s: %w", o.Table, err)
	}
	if tag.RowsAffected() == 0 {
		// The owning row went between the read that authorized this and the
		// write. Rolling the transaction back is what leaves the file row
		// pending, and pending is reapable.
		return ErrNotFound
	}
	return nil
}

// pending lists file rows whose bytes never arrived.
func (s store) pending(ctx context.Context, before time.Time, limit int) ([]*File, error) {
	const q = `SELECT ` + columns + ` FROM ` + Table + `
		WHERE uploaded_at IS NULL AND created_at < $1
		ORDER BY created_at LIMIT $2`

	return s.list(ctx, q, before, limit)
}

// expired lists file rows whose restore window has closed.
func (s store) expired(ctx context.Context, before time.Time, limit int) ([]*File, error) {
	const q = `SELECT ` + columns + ` FROM ` + Table + `
		WHERE deleted_at IS NOT NULL AND deleted_at < $1
		ORDER BY deleted_at LIMIT $2`

	return s.list(ctx, q, before, limit)
}

// trashed lists file rows that are deleted and still inside the window, which is
// what the sweeper re-marks against the bucket.
func (s store) trashed(ctx context.Context, since time.Time, limit int) ([]*File, error) {
	const q = `SELECT ` + columns + ` FROM ` + Table + `
		WHERE deleted_at IS NOT NULL AND deleted_at >= $1
		ORDER BY deleted_at LIMIT $2`

	return s.list(ctx, q, since, limit)
}

// hardDelete removes the row once the bytes are gone.
func (s store) hardDelete(ctx context.Context, id uuid.UUID) error {
	if _, err := s.conn(ctx).Exec(ctx, `DELETE FROM `+Table+` WHERE id = $1`, id); err != nil {
		return fmt.Errorf("files: remove file row: %w", err)
	}
	return nil
}

func (s store) list(ctx context.Context, q string, at time.Time, limit int) ([]*File, error) {
	rows, err := s.conn(ctx).Query(ctx, q, at, limit)
	if err != nil {
		return nil, fmt.Errorf("files: list: %w", err)
	}
	defer rows.Close()

	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// scanner is what both QueryRow and Rows offer, so one scan serves the single
// read and the listing.
type scanner interface{ Scan(dest ...any) error }

func scanFile(row scanner) (*File, error) {
	var (
		f        File
		url      *string
		declared *string
		checksum *string
	)
	err := row.Scan(
		&f.ID, &f.TenantID, &f.StorageKey, &url, &f.Name, &f.ContentType,
		&declared, &f.Size, &checksum, &f.CreatedAt, &f.UploadedAt, &f.DeletedAt,
	)
	switch {
	case dbx.IsNoRows(err):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("files: read file: %w", err)
	}

	f.URL = deref(url)
	f.DeclaredType = deref(declared)
	f.Checksum = deref(checksum)
	return &f, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// quoteIdent renders a table or column name for a statement.
//
// The names it is given are written by a generator from the document and are
// constants in the generated source; none of them comes from a request. The
// check is here anyway, because "no request reaches this" is a property of every
// caller rather than of this function, and a panic on an identifier nobody could
// have typed is cheaper than trusting that forever.
func quoteIdent(name string) string {
	for _, r := range name {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			panic("files: refusing to build a statement around the identifier " + name)
		}
	}
	return `"` + name + `"`
}

// errNoStore is what a Service built without a backend answers, rather than
// panicking somewhere further in.
var errNoStore = errors.New("files: no blob store configured")
