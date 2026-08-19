package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
)

func TestPutReportsWhatItWrote(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()

	content := []byte("the quick brown fox")
	info, err := s.Put(ctx, "k", bytes.NewReader(content), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if info.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", info.Size, len(content))
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); info.Checksum != want {
		t.Errorf("checksum = %q, want %q", info.Checksum, want)
	}
	if info.ModTime.IsZero() {
		t.Error("no modification time, so ServeContent has nothing to answer a conditional request with")
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()
	content := []byte("hello")

	if _, err := s.Put(ctx, "k", bytes.NewReader(content), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	r, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read back %q, want %q", got, content)
	}
}

// The download route answers ranges, which is why Get returns a seeker and why
// Store has Stat at all. Without it a video cannot be scrubbed and a resumed
// download starts over.
func TestRangeReadsAtTheBoundaries(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()
	content := []byte("0123456789")

	if _, err := s.Put(ctx, "k", bytes.NewReader(content), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		offset int64
		whence int
		want   string
	}{
		{"from the start", 0, io.SeekStart, "0123456789"},
		{"from the middle", 4, io.SeekStart, "456789"},
		{"the last byte", -1, io.SeekEnd, "9"},
		{"past the end", 10, io.SeekStart, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.Get(ctx, "k")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			if _, err := r.Seek(tc.offset, tc.whence); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("read %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMissingObject(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get: %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, "nope"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat: %v, want ErrNotFound", err)
	}
}

// The sweeper runs over whatever the table says is expired, and a second pass
// after a crash goes over rows it already handled. If that were an error, every
// caller would need to tell "already gone" apart from "could not delete".
func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()

	if _, err := s.Put(ctx, "k", strings.NewReader("x"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Errorf("deleting twice should be fine: %v", err)
	}
	if err := s.Delete(ctx, "never existed"); err != nil {
		t.Errorf("deleting nothing should be fine: %v", err)
	}
}

// The whole reason the key is derived rather than supplied. A filename is data
// and belongs in the row; the moment it is part of a path it is a question
// about traversal, casing and absolute paths that has to be got right
// everywhere it is used.
func TestKeyIsDerivedFromTheIdentifierAlone(t *testing.T) {
	id := uuid.MustParse("0192a3b4-c5d6-7e8f-9012-3456789abcde")
	key := blob.Key(id)

	if !strings.Contains(key, id.String()) {
		t.Errorf("key %q does not contain the identifier", key)
	}
	for _, bad := range []string{"..", "//", "\\"} {
		if strings.Contains(key, bad) {
			t.Errorf("key %q contains %q", key, bad)
		}
	}

	// The same identifier always gives the same key, or a download cannot find
	// what an upload wrote.
	if again := blob.Key(id); again != key {
		t.Errorf("key is not stable: %q then %q", key, again)
	}

	// And two identifiers never collide, which is what makes the key safe to
	// derive without consulting anything.
	seen := map[string]bool{}
	for range 1000 {
		k := blob.Key(uuid.New())
		if seen[k] {
			t.Fatalf("duplicate key %q", k)
		}
		seen[k] = true
	}
}

// A name like this never reaches the store, because Key never sees it. The test
// is here so that a later change routing the filename into the key fails
// something rather than passing quietly.
func TestAHostileFilenameCannotEscape(t *testing.T) {
	id := uuid.New()
	key := blob.Key(id)

	for _, name := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"..\\..\\windows\\system32",
		strings.Repeat("../", 40) + "etc/passwd",
	} {
		if strings.Contains(key, name) {
			t.Errorf("key %q contains the supplied name %q", key, name)
		}
	}
}

// The mark is a projection of the row, and the row leads. These are the
// assertions the generated delete and restore paths rely on.
func TestMarking(t *testing.T) {
	ctx := context.Background()
	s := blob.NewMemory()
	at := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	if _, err := s.Put(ctx, "k", strings.NewReader("x"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	// An object arrives live, so an unmarked bucket and a table with nothing
	// deleted in it agree from the start.
	if state, _, ok := s.State("k"); !ok || state != blob.StateLive {
		t.Errorf("state = %q (found %v), want live", state, ok)
	}

	if err := s.Mark(ctx, "k", blob.StateDeleted, at); err != nil {
		t.Fatal(err)
	}
	state, marked, ok := s.State("k")
	if !ok || state != blob.StateDeleted {
		t.Errorf("state = %q, want deleted", state)
	}
	if !marked.Equal(at) {
		t.Errorf("marked at %v, want %v", marked, at)
	}

	// Restore clears it, the same way and in the same direction.
	if err := s.Mark(ctx, "k", blob.StateLive, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := s.State("k"); state != blob.StateLive {
		t.Errorf("state = %q after restore, want live", state)
	}

	if err := s.Mark(ctx, "gone", blob.StateDeleted, at); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("marking a missing object: %v, want ErrNotFound", err)
	}
}

// Memory implements Marker and not Signer, and a caller decides what a backend
// can do by asking rather than by calling and handling an error.
func TestOptionalInterfacesAreDetectable(t *testing.T) {
	var s blob.Store = blob.NewMemory()

	if _, ok := s.(blob.Marker); !ok {
		t.Error("Memory should mark, so the delete path can be tested against it")
	}
	if _, ok := s.(blob.Signer); ok {
		t.Error("Memory should not sign: there is no URL to send a client to")
	}
}
