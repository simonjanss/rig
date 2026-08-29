package blob_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
)

// The whole seam in one pass: write bytes, learn what they weighed and hashed
// to, read them back. The checksum comes back from the write rather than being
// handed to it, which is why a rig_file row is finalized after the upload
// rather than before — until the last byte has gone past there is nothing to
// record.
func Example() {
	ctx := context.Background()
	store := blob.NewMemory()

	info, err := store.Put(ctx, "files/ab/cd/abcd", strings.NewReader("hello"), blob.PutOptions{
		ContentType: "text/plain",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(info.Size, info.Checksum)

	r, err := store.Get(ctx, "files/ab/cd/abcd")
	if err != nil {
		panic(err)
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", body)

	// Output:
	// 5 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	// hello
}

// The key is derived from the file's identifier and nothing else. The name a
// client uploaded stays in the row, where it is data: the moment it is part of
// a path, traversal and casing and absolute paths are all somebody's problem to
// get right everywhere the key is used.
//
// The two-level prefix is for the backends that shard by key prefix, and costs
// nothing on the ones that do not.
func ExampleKey() {
	id := uuid.MustParse("0192a3b4-c5d6-7e8f-9012-3456789abcde")

	fmt.Println(blob.Key(id))

	// Output:
	// files/01/92/0192a3b4-c5d6-7e8f-9012-3456789abcde
}

// A missing object is a sentinel rather than a typed error, because the only
// thing a caller ever needs to know is which of the two cases it has — and
// errors.Is is how the rest of the standard library asks that question.
func ExampleErrNotFound() {
	_, err := blob.NewMemory().Get(context.Background(), "files/00/00/gone")

	fmt.Println(errors.Is(err, blob.ErrNotFound))

	// Output:
	// true
}

// Get returns a ReadSeekCloser so a download can answer a range request:
// http.ServeContent needs to seek, and without it a video cannot be scrubbed
// and a resumed download starts from the beginning.
func ExampleStore_range() {
	ctx := context.Background()
	store := blob.NewMemory()

	if _, err := store.Put(ctx, "k", strings.NewReader("0123456789"), blob.PutOptions{}); err != nil {
		panic(err)
	}

	r, err := store.Get(ctx, "k")
	if err != nil {
		panic(err)
	}
	defer r.Close()

	if _, err := r.Seek(4, io.SeekStart); err != nil {
		panic(err)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", rest)

	// Output:
	// 456789
}

// A backend says what it can do by having the method, so a caller asks rather
// than calling and handling an error in production. Memory can record a
// deletion on the object; it cannot mint a URL, and there is no URL it could
// usefully send a client to.
func ExampleMarker() {
	var store blob.Store = blob.NewMemory()

	_, marks := store.(blob.Marker)
	_, signs := store.(blob.Signer)
	fmt.Println(marks, signs)

	// Output:
	// true false
}

// The row leads and the mark follows: set deleted_at, commit, then mark. A
// failed mark leaves a deleted row beside an unmarked object, which is the safe
// direction — the sweeper still knows from the row. Marking first would produce
// an object tagged deleted that the database says is live, and nothing
// reconciles that back.
func ExampleMemory_Mark() {
	ctx := context.Background()
	store := blob.NewMemory()
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	if _, err := store.Put(ctx, "k", strings.NewReader("x"), blob.PutOptions{}); err != nil {
		panic(err)
	}
	state, _, _ := store.State("k")
	fmt.Println(state)

	if err := store.Mark(ctx, "k", blob.StateDeleted, at); err != nil {
		panic(err)
	}
	state, marked, _ := store.State("k")
	fmt.Println(state, marked.Format(time.RFC3339))

	// Output:
	// live
	// deleted 2026-08-19T12:00:00Z
}

// Delete says nothing about a key that was already gone. That is what lets the
// sweeper be a simple loop over whatever the table says is expired: a second
// pass after a crash is not an error anybody has to tell apart from a real one.
func ExampleMemory_Delete() {
	ctx := context.Background()
	store := blob.NewMemory()

	if _, err := store.Put(ctx, "k", strings.NewReader("x"), blob.PutOptions{}); err != nil {
		panic(err)
	}
	fmt.Println(store.Delete(ctx, "k"))
	fmt.Println(store.Delete(ctx, "k"))
	fmt.Println(store.Delete(ctx, "never existed"))

	// Output:
	// <nil>
	// <nil>
	// <nil>
}
