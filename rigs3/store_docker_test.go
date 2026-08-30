//go:build docker

// The adapter against a real object store.
//
//	go test -tags docker ./rigs3/
//
// Everything this module does is a claim about somebody else's server, and a
// fake would answer whatever it was written to answer. Four of these cannot be
// made any other way: that a presigned URL is one a client can really PUT to,
// that a tag survives being rewritten by the next mark, that an upload past the
// SDK's part size still hashes to the whole object rather than to a checksum of
// checksums, and that the lifecycle read finds a rule a bucket actually holds.
//
// The rest re-derives the case list in files/blob/blob_test.go, which is the
// contract blob.Memory is held to and the one thing that says these two
// backends behave alike.
//
// Every test gets a bucket of its own. Objects would be enough for most of
// them, but a lifecycle rule belongs to a bucket, and one test's rule showing
// up in another's read would be a failure nobody could place.
package rigs3

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/files/blob"
)

func TestPutReportsWhatItWrote(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("hello world")

	info, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}

	if info.Size != int64(len(content)) {
		t.Errorf("Size is %d, want %d", info.Size, len(content))
	}
	if want := sha256Of(content); info.Checksum != want {
		t.Errorf("Checksum is %q, want %q", info.Checksum, want)
	}
	if info.ModTime.IsZero() {
		t.Error("no modification time, so ServeContent has nothing to answer a conditional request with")
	}
}

// Stat has to agree with Put, or the checksum in a rig_file row and the one the
// bucket can be asked for are two different numbers.
func TestStatAgreesWithPut(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("the quick brown fox")

	put, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stat, err := s.Stat(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}

	if stat.Size != put.Size {
		t.Errorf("Stat says %d bytes and Put said %d", stat.Size, put.Size)
	}
	if stat.Checksum != put.Checksum {
		t.Errorf("Stat says %q and Put said %q", stat.Checksum, put.Checksum)
	}
	if stat.ModTime.IsZero() {
		t.Error("Stat reports no modification time")
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("bytes in, bytes out")

	if _, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	r, err := s.Get(t.Context(), key)
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

// The four seeks http.ServeContent makes, against a bucket that has no such
// thing as a seek. This is the whole reason Get returns a ReadSeekCloser.
func TestRangeReadsAtTheBoundaries(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("0123456789")

	if _, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		offset int64
		whence int
		want   string
	}{
		{"from the start", 0, io.SeekStart, "0123456789"},
		{"from the middle", 4, io.SeekStart, "456789"},
		{"the last byte", -1, io.SeekEnd, "9"},
		{"past the end", 20, io.SeekStart, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := s.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			if _, err := r.Seek(c.offset, c.whence); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reading after the seek: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("read %q, want %q", got, c.want)
			}
		})
	}
}

// Seeking backwards means opening a second stream, which is the one thing this
// reader does that a file does not have to.
func TestSeekingBackwardsReadsAgain(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())

	if _, err := s.Put(t.Context(), key, strings.NewReader("0123456789"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	r, err := s.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	first := make([]byte, 4)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	again := make([]byte, 4)
	if _, err := io.ReadFull(r, again); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, again) {
		t.Errorf("the second read from the start gave %q, want %q", again, first)
	}
}

func TestMissingObject(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())

	if _, err := s.Get(t.Context(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get of a missing key gave %v, want blob.ErrNotFound", err)
	}
	if _, err := s.Stat(t.Context(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat of a missing key gave %v, want blob.ErrNotFound", err)
	}
}

// Idempotence is what lets the sweeper be a loop rather than a state machine.
func TestDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())

	if _, err := s.Put(t.Context(), key, strings.NewReader("x"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := s.Delete(t.Context(), key); err != nil {
			t.Fatalf("delete %d: %v", i+1, err)
		}
	}
	if err := s.Delete(t.Context(), blob.Key(uuid.New())); err != nil {
		t.Errorf("deleting a key that was never there: %v", err)
	}
	if _, err := s.Get(t.Context(), key); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the object is still readable after being deleted: %v", err)
	}
}

// An object arrives live and carrying its checksum, which is what makes a
// bucket audit possible at all.
func TestAnObjectArrivesLiveAndHashed(t *testing.T) {
	t.Parallel()

	s, bucket := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("tagged on the way in")

	info, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tags := tagsOf(t, s, bucket, key)
	if tags[tagState] != string(blob.StateLive) {
		t.Errorf("%s is %q, want %q", tagState, tags[tagState], blob.StateLive)
	}
	if tags[tagChecksum] != info.Checksum {
		t.Errorf("%s is %q, want %q", tagChecksum, tags[tagChecksum], info.Checksum)
	}
	if tags[tagMarkedAt] == "" {
		t.Errorf("%s is empty, so the bucket cannot say when this was last written", tagMarkedAt)
	}
}

// Marking rewrites the tag set, and S3 has no way to set one tag and leave the
// rest. Losing the checksum here would be losing it for good.
func TestMarkingKeepsTheChecksum(t *testing.T) {
	t.Parallel()

	s, bucket := store(t, 0)
	key := blob.Key(uuid.New())

	info, err := s.Put(t.Context(), key, strings.NewReader("marked"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	deletedAt := time.Now().UTC().Truncate(time.Second)
	if err := s.Mark(t.Context(), key, blob.StateDeleted, deletedAt); err != nil {
		t.Fatal(err)
	}

	tags := tagsOf(t, s, bucket, key)
	if tags[tagState] != string(blob.StateDeleted) {
		t.Errorf("%s is %q after a delete, want %q", tagState, tags[tagState], blob.StateDeleted)
	}
	if tags[tagChecksum] != info.Checksum {
		t.Errorf("the checksum did not survive the mark: %q, want %q", tags[tagChecksum], info.Checksum)
	}
	if got := tags[tagMarkedAt]; got != deletedAt.Format(time.RFC3339) {
		t.Errorf("%s is %q, want %q", tagMarkedAt, got, deletedAt.Format(time.RFC3339))
	}

	// A restore clears it the same way and in the same direction.
	if err := s.Mark(t.Context(), key, blob.StateLive, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if tags := tagsOf(t, s, bucket, key); tags[tagState] != string(blob.StateLive) {
		t.Errorf("%s is %q after a restore, want %q", tagState, tags[tagState], blob.StateLive)
	}
}

func TestMarkingSomethingThatIsNotThere(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	err := s.Mark(t.Context(), blob.Key(uuid.New()), blob.StateDeleted, time.Now())
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("marking a missing key gave %v, want blob.ErrNotFound", err)
	}
}

// Past the SDK's five-megabyte part size the upload is multipart, and the ETag
// stops being a hash of the object. This is the case blob.Info's doc comment is
// about, and the reason the checksum is rig's own rather than the bucket's.
func TestAMultipartUploadHashesTheWholeObject(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())

	content := bytes.Repeat([]byte("rig"), 4*1024*1024) // 12 MiB, so three parts

	info, err := s.Put(t.Context(), key, bytes.NewReader(content), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("Size is %d, want %d", info.Size, len(content))
	}
	if want := sha256Of(content); info.Checksum != want {
		t.Errorf("Checksum is %q, want the hash of the whole object %q", info.Checksum, want)
	}

	// And it reads back byte for byte, which a badly assembled multipart upload
	// would not.
	r, err := s.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("the object did not come back byte for byte")
	}
}

// A signed URL is a claim about somebody else's server accepting a request rig
// never makes. Nothing short of making it proves anything.
func TestASignedUploadIsOneAClientCanUse(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)
	key := blob.Key(uuid.New())
	content := []byte("uploaded by the client")

	url, err := s.SignUpload(t.Context(), key, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the signed PUT was answered with %d", res.StatusCode)
	}

	r, err := s.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("the object the client uploaded is not readable: %v", err)
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

func TestASignedUploadStopsWorking(t *testing.T) {
	t.Parallel()

	s, _ := store(t, 0)

	url, err := s.SignUpload(t.Context(), blob.Key(uuid.New()), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader("too late"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("an expired signature was accepted, so the expiry is decoration")
	}
}

// The check the whole startup cost is for: a bucket that would take the bytes
// before the restore window closes is one rig will not serve from.
func TestABucketThatExpiresSoonerThanTheWindowIsRefused(t *testing.T) {
	t.Parallel()

	s, bucket := store(t, 0)
	expireAfter(t, s, bucket, 7)

	_, err := New(t.Context(), config(t, bucket, 30*24*time.Hour))
	if err == nil {
		t.Fatal("a bucket that expires objects in seven days was accepted under a thirty-day window")
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("the refusal does not say what the bucket is set to: %v", err)
	}
}

func TestABucketThatOutlivesTheWindowIsAccepted(t *testing.T) {
	t.Parallel()

	s, bucket := store(t, 0)
	expireAfter(t, s, bucket, 90)

	if _, err := New(t.Context(), config(t, bucket, 30*24*time.Hour)); err != nil {
		t.Fatalf("a ninety-day rule under a thirty-day window was refused: %v", err)
	}
}

// A bucket with no lifecycle configuration at all is the ordinary case, and the
// error S3 answers that read with must not be mistaken for a failure.
func TestABucketWithNoLifecycleAtAllIsAccepted(t *testing.T) {
	t.Parallel()

	_, bucket := store(t, 0)

	if _, err := New(t.Context(), config(t, bucket, 30*24*time.Hour)); err != nil {
		t.Fatalf("a bucket with no lifecycle rules was refused: %v", err)
	}
}
