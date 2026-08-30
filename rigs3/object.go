package rigs3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/simonjanss/rig/files/blob"
)

// object is one open object, as an [io.ReadSeekCloser].
//
// S3 has no such thing — a GET is a stream from an offset to the end, and there
// is no going back — so this is the seam where a stateless protocol is made to
// look like a file. Two rules do it, and both come from what the callers
// actually do.
//
// Seeking is arithmetic. [http.ServeContent] seeks to the end and back to the
// start before it serves a byte, purely to learn the length, and a Seek that
// fetched anything would make that two wasted round trips on every download.
// The size was learned once by the Stat that opened this.
//
// The body follows the offset lazily. Reading after a seek that moved somewhere
// the open stream cannot reach closes it and opens another at the new offset,
// so a sequential read is one GET and a scrubbed video is one per scrub.
type object struct {
	// ctx is the context Get was called with. Holding one in a struct is
	// usually wrong; here the alternative is a Read that cannot be cancelled,
	// because io.ReadSeekCloser has nowhere to put a second one.
	ctx    context.Context
	api    *s3.Client
	bucket string
	key    string

	// size is what the object was when it was opened. A concurrently replaced
	// object is out of scope for the same reason it is everywhere else in rig:
	// a file is immutable, and a replacement is a new key.
	size int64
	// off is where the next Read starts.
	off int64

	// body is the open stream, and bodyOff is where it will read from next.
	// Both nil and zero when there is no stream open.
	body    io.ReadCloser
	bodyOff int64
}

// Read fills p from the current offset, opening a stream if the one that is
// open cannot answer from there.
func (o *object) Read(p []byte) (int, error) {
	if o.off >= o.size {
		return 0, io.EOF
	}
	if o.body != nil && o.bodyOff != o.off {
		o.closeBody()
	}
	if o.body == nil {
		if err := o.open(); err != nil {
			return 0, err
		}
	}

	n, err := o.body.Read(p)
	o.off += int64(n)
	o.bodyOff += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		// A broken stream is not a broken object: drop it so the next read
		// asks again from where this one stopped.
		o.closeBody()
	}
	return n, err
}

// Seek moves the offset and asks the bucket nothing.
//
// Past the end is allowed, and reads there report EOF rather than the 416 a
// ranged GET would — which is what an [io.ReaderAt] over a file does, and what
// [blob.Store] therefore has to.
func (o *object) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = o.off + offset
	case io.SeekEnd:
		abs = o.size + offset
	default:
		return 0, fmt.Errorf("rigs3: seek %s: unknown whence %d", o.key, whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("rigs3: seek %s to %d: before the start of the object", o.key, abs)
	}
	o.off = abs
	return abs, nil
}

// Close releases the stream, if one is open. Closing twice is not an error,
// because a handler that defers a Close beside one it already did is a handler
// that should still work.
func (o *object) Close() error {
	o.closeBody()
	return nil
}

// open starts a ranged GET at the current offset.
func (o *object) open() error {
	out, err := o.api.GetObject(o.ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.bucket),
		Key:    aws.String(o.key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-", o.off)),
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("rigs3: %s in bucket %s: %w", o.key, o.bucket, blob.ErrNotFound)
		}
		return fmt.Errorf("rigs3: read %s from bucket %s: %w", o.key, o.bucket, err)
	}
	o.body = out.Body
	o.bodyOff = o.off
	return nil
}

func (o *object) closeBody() {
	if o.body != nil {
		_ = o.body.Close()
		o.body = nil
		o.bodyOff = 0
	}
}
