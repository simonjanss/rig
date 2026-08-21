// Package idempotency makes a write that had to be sent twice mean one write.
//
// A client that never gets an answer cannot tell a request that was lost on the
// way out from one that was applied and whose response was lost on the way back.
// Retrying is right for the first and wrong for the second, and nothing the
// client holds tells them apart — so the server has to remember, and
// Idempotency-Key is the name the client gives to what it wants remembered.
//
// [Run] is the whole of it. The record of the write and the write itself commit
// together, which is the property everything else here follows from: a claim
// stored separately from the effect it claims can outlive a rolled-back write,
// and then a key that wrote nothing replays a success forever.
//
// The table is rig_idempotency, created by
// [github.com/simonjanss/rig/runtime/foundation].
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// Table is where records are kept. Named here as well as in the migration
// because the queries below are the only code that reads it, and a constant is
// what makes that greppable.
const Table = "rig_idempotency"

// LockTimeout bounds how long a request waits for another holding the same key.
//
// Two requests with one key is a client that retried before the first attempt
// finished, so the second one is usually about to be thrown away by a caller who
// has already stopped listening. Waiting is still the right default — the answer
// it gets is the answer it wanted — but waiting without a limit would let a slow
// write pin a connection per retry, and a retry storm is exactly when there are
// fewest to spare.
//
// It is a constant rather than configuration because it is bounded on both sides
// by things a project does not choose: shorter than the server's write timeout,
// longer than any write worth deduplicating.
const LockTimeout = 5 * time.Second

// DefaultRetention is how long a record is kept, and so how long after a write
// its key still replays.
//
// A day, because the thing a key protects against is a retry, and a retry that
// arrives a day later is not a retry — it is a second request that happens to
// reuse an identifier. Keeping them longer would turn a client's stale key into
// a write that silently does nothing.
const DefaultRetention = 24 * time.Hour

// Request is what identifies one write for this purpose.
type Request struct {
	// TenantID scopes the key. Two tenants choosing the same string is not a
	// collision, and one tenant's key can never replay another's response.
	TenantID uuid.UUID

	// Key is what the client sent. Empty means the client asked for none of
	// this, and [Run] does nothing but call the write.
	Key string

	// Endpoint is the route pattern, not the path. Part of the identity so that
	// one client-side identifier reused across two endpoints is two records
	// rather than a replay of the wrong one.
	Endpoint string

	// Fingerprint is what [Fingerprint] made of the request body. A second
	// request with the same key and a different body is refused rather than
	// answered.
	Fingerprint []byte

	// RequestID is this request's own identifier, recorded so that a replay can
	// name the request that did the work.
	RequestID string
}

// Result is what a write answered with, whether it just ran or is a replay.
type Result struct {
	// Status is the HTTP status to answer with.
	Status int

	// Body is the response, ready to write. Nil where there was none, which is
	// what a 204 answers with.
	Body json.RawMessage

	// Replayed says this is a record of an earlier write rather than a new one.
	// The caller has nothing it must do about it; it is worth a response header
	// and worth a log line.
	Replayed bool
}

// Write is the work [Run] guards: the handler's own call into its service,
// answering with the status it would have written and the value it would have
// encoded.
//
// The context it is given is the one carrying the transaction, and passing it on
// is what puts the service's write in the same transaction as the record of it.
type Write func(ctx context.Context) (status int, body any, err error)

// Run performs a write once, however many times it is asked for.
//
// With no key it calls the write and nothing else — no transaction, no row, no
// second round trip. That is the common path and it costs what it always cost,
// which is what makes this safe to have on every write endpoint rather than on
// the ones somebody remembered to configure.
//
// With a key it opens a transaction, claims the key, runs the write, and records
// what it answered — all before committing, so the record and the effect are one
// fact. A second request holding the same key blocks on the claim until this one
// ends, and then either replays what it committed or, if it rolled back, does the
// work itself.
//
// A write that fails leaves no record. The transaction rolls back and the key is
// free again, which is the deliberate reading of what a failed write means: it
// wrote nothing, so there is nothing to be idempotent about, and a caller who
// fixes their body and reuses the key gets the write they wanted rather than a
// cached complaint about the old one.
func Run(ctx context.Context, db dbx.Beginner, req Request, write Write) (Result, error) {
	if req.Key == "" {
		status, body, err := write(ctx)
		if err != nil {
			return Result{}, err
		}
		encoded, err := encode(body)
		if err != nil {
			return Result{}, err
		}
		return Result{Status: status, Body: encoded}, nil
	}

	var out Result
	err := dbx.InTx(ctx, db, func(ctx context.Context, tx dbx.Conn) error {
		// LOCAL, so the pooled connection goes back the way it came rather than
		// carrying a five-second ceiling into whatever request has it next.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
			LockTimeout.Milliseconds())); err != nil {
			return rigerr.Internal(err, "bounding the wait for an idempotency key")
		}

		claimed, err := claim(ctx, tx, req)
		if err != nil {
			return err
		}

		// The ceiling was for the claim and the claim is over. SET LOCAL lasts the
		// rest of the transaction, so leaving it in place would put five seconds on
		// every lock the write goes on to take — and a write that waits on a
		// contended row has not gone wrong, it is waiting. The wrong shape of that
		// is a 500 where there used to be a slow 201.
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = DEFAULT"); err != nil {
			return rigerr.Internal(err, "restoring the lock timeout after an idempotency claim")
		}

		if !claimed {
			// Somebody got here first and committed. Whatever they answered is
			// this caller's answer too.
			out, err = replay(ctx, tx, req)
			return err
		}

		status, body, err := write(ctx)
		if err != nil {
			return err
		}
		encoded, err := encode(body)
		if err != nil {
			return err
		}
		if err := record(ctx, tx, req, status, encoded); err != nil {
			return err
		}
		out = Result{Status: status, Body: encoded}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return out, nil
}

// claim inserts the record this write will fill in, and reports whether it is
// this request's to fill.
//
// The conflict is the interesting half. A row committed by an earlier request
// means false and a replay; a row an in-flight request has inserted and not yet
// committed means this INSERT blocks, and when that transaction ends it either
// conflicts with what it committed or succeeds because it rolled back. So there
// is no third state to handle, and no lease or TTL to invent: Postgres is
// already keeping exactly this bookkeeping for its own reasons.
func claim(ctx context.Context, tx dbx.Conn, req Request) (bool, error) {
	const q = `
		INSERT INTO ` + Table + ` (tenant_id, key, endpoint, fingerprint, request_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, key, endpoint) DO NOTHING`

	tag, err := tx.Exec(ctx, q, req.TenantID, req.Key, req.Endpoint, req.Fingerprint, req.RequestID)
	if err != nil {
		if lockTimedOut(err) {
			return false, rigerr.Conflict(
				"another request is still using this idempotency key; retry in a moment").Wrap(err)
		}
		return false, rigerr.Internal(err, "claiming an idempotency key")
	}
	return tag.RowsAffected() == 1, nil
}

// replay reads back what the write answered the first time.
func replay(ctx context.Context, tx dbx.Conn, req Request) (Result, error) {
	const q = `
		SELECT fingerprint, status, response
		FROM ` + Table + `
		WHERE tenant_id = $1 AND key = $2 AND endpoint = $3`

	var (
		fingerprint []byte
		status      *int
		// Scanned as bytes rather than as a json.RawMessage: the column is text,
		// and asking pgx for a type that implements json.Unmarshaler invites it
		// to parse what this package went to some trouble not to reshape.
		body []byte
	)
	err := tx.QueryRow(ctx, q, req.TenantID, req.Key, req.Endpoint).
		Scan(&fingerprint, &status, &body)
	switch {
	case dbx.IsNoRows(err):
		// The claim conflicted, so the row was there a statement ago. Reaching
		// here means somebody deleted it in between — a retention sweep with a
		// window of nothing, most likely — and the honest answer is that this
		// request should be sent again rather than answered from a record that
		// no longer exists.
		return Result{}, rigerr.Conflict(
			"the record for this idempotency key went away while it was being read; send the request again")
	case err != nil:
		return Result{}, rigerr.Internal(err, "reading an idempotency record")
	}

	if !bytes.Equal(fingerprint, req.Fingerprint) {
		// Replaying the wrong response is worse than any error: the caller would
		// get a success describing something it did not ask for, and would have
		// no way to tell.
		return Result{}, rigerr.Invalid(
			"this idempotency key was used for a different request; use a new key for a new request")
	}
	if status == nil {
		// Null only inside the transaction that is about to fill it in, and that
		// transaction is not this one. A committed row with no status would mean
		// the invariant this package is built on has been broken.
		return Result{}, rigerr.Internal(nil,
			"an idempotency record committed without a status")
	}
	return Result{Status: *status, Body: body, Replayed: true}, nil
}

// record fills in what the write answered, in the transaction that performed it.
func record(ctx context.Context, tx dbx.Conn, req Request, status int, body json.RawMessage) error {
	const q = `
		UPDATE ` + Table + `
		SET status = $4, response = $5
		WHERE tenant_id = $1 AND key = $2 AND endpoint = $3`

	if _, err := tx.Exec(ctx, q, req.TenantID, req.Key, req.Endpoint, status, nullable(body)); err != nil {
		return rigerr.Internal(err, "recording what an idempotent write answered")
	}
	return nil
}

// Prune deletes records past their retention, and reports how many.
//
// A subcommand rather than a goroutine, like the file sweep and the notification
// dispatch: a cron job is one thing running, and a goroutine in every replica is
// as many as there are replicas, all racing to delete the same rows.
func Prune(ctx context.Context, db dbx.Conn, retention time.Duration) (int64, error) {
	const q = `DELETE FROM ` + Table + ` WHERE created_at < $1`

	if retention <= 0 {
		retention = DefaultRetention
	}
	tag, err := db.Exec(ctx, q, time.Now().UTC().Add(-retention))
	if err != nil {
		return 0, rigerr.Internal(err, "pruning idempotency records")
	}
	return tag.RowsAffected(), nil
}

// Fingerprint is what a request body reduces to for the purpose of noticing that
// a key has been reused for something else.
//
// For a multipart write it is given the fields and not the file bytes. Hashing
// an upload would mean buffering a file that may be larger than memory, which is
// the one thing the whole file path is built to avoid — so the guarantee here is
// narrower than it looks: two uploads that differ only in their bytes have the
// same fingerprint. That is the trade, and it is the right way round, because
// the failure it admits is a client reusing a key across two different uploads,
// which is a client bug, and the failure it avoids is an out-of-memory, which is
// an outage.
func Fingerprint(body any) []byte {
	if body == nil {
		return sha256sum(nil)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		// A body that will not encode is about to fail the write anyway. A
		// fingerprint of the failure is stable, which is all this needs to be.
		return sha256sum([]byte(err.Error()))
	}
	return sha256sum(encoded)
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// encode turns what the write answered into the bytes that will be both written
// and stored.
//
// Once, deliberately. Marshalling for the record and again for the response
// would be the same work twice and, worse, two chances to differ.
func encode(body any) (json.RawMessage, error) {
	if body == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, rigerr.Internal(err, "encoding a response")
	}
	return encoded, nil
}

// nullable keeps an absent body absent rather than storing the four bytes that
// spell null, so that a 204 reads back as no body at all.
//
// A string rather than the raw message, because the column is text and handing
// pgx a []byte for a text column is asking it to choose between two readings of
// the same value.
func nullable(body json.RawMessage) any {
	if len(body) == 0 {
		return nil
	}
	s := string(body)
	return &s
}

// lockTimedOut reports the one Postgres failure this package expects: another
// transaction held the key for longer than [LockTimeout].
func lockTimedOut(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}
