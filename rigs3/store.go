package rigs3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/simonjanss/rig/files/blob"
)

// Put streams the object into the bucket and reports what went past.
//
// The reader is read once, through a hash, which is what makes an upload larger
// than memory an ordinary upload: nothing here holds the object, and the parts
// the SDK buffers are bounded by its part size. It also matters that the reader
// is read exactly once and never rewound — the one rig hands over refuses
// rather than truncating at the byte cap, and a retry that re-read it would be
// asking a request body to happen twice.
//
// # Why the checksum is a tag
//
// [blob.Info.Checksum] is a SHA-256 that cannot be known until the last byte
// has gone past, and object metadata is fixed when the upload begins. So it is
// written afterwards, as an object tag: one cheap call, no size limit, and it
// does not rewrite the object.
//
// The two alternatives are worse. Copying the object onto itself to replace its
// metadata needs a multipart copy past 5 GiB. And S3's own SHA-256 is a
// checksum of checksums as soon as an upload is multipart, which is the ETag
// problem [blob.Info] exists to avoid, stated again in a different field.
//
// A failed tagging fails the Put, rather than leaving an object rig cannot
// answer questions about. The cost is an upload thrown away; what it buys is
// that every object in the bucket carries its checksum and its state, which is
// the whole reason [Store.Mark] exists. The row it belongs to is still pending
// at this point, so the sweeper's first rule reaps what is left behind.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opt blob.PutOptions) (blob.Info, error) {
	counted := &hashed{sum: sha256.New()}

	in := &transfermanager.UploadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   io.TeeReader(r, counted),
	}
	if opt.ContentType != "" {
		in.ContentType = aws.String(opt.ContentType)
	}
	if _, err := s.uploader.UploadObject(ctx, in); err != nil {
		// Not s.wrap: a bucket that is not there answers a PutObject with a 404,
		// and reporting that as blob.ErrNotFound would be this store claiming an
		// object it was asked to write does not exist.
		return blob.Info{}, fmt.Errorf("rigs3: write %s to bucket %s: %w", key, s.bucket, err)
	}

	checksum := hex.EncodeToString(counted.sum.Sum(nil))
	now := time.Now().UTC()

	// The whole tag set in one call, because at this point it is known: an
	// object arrives live, the way one does in blob.Memory.
	if err := s.putTags(ctx, key, map[string]string{
		tagChecksum: checksum,
		tagState:    string(blob.StateLive),
		tagMarkedAt: now.Format(time.RFC3339),
	}); err != nil {
		return blob.Info{}, err
	}

	// Counted here rather than asked of the bucket: a HeadObject would be a
	// third round trip to learn a number this call has already watched go past,
	// and blob.Memory reports the same two the same way.
	return blob.Info{Size: counted.n, Checksum: checksum, ModTime: now}, nil
}

// hashed is the sink Put tees the upload into: the checksum and the length, in
// one pass, because the reader on the other side is a request body and there is
// no second pass to be had.
type hashed struct {
	sum hash.Hash
	n   int64
}

func (h *hashed) Write(p []byte) (int, error) {
	h.n += int64(len(p))
	return h.sum.Write(p)
}

// Get opens the object for reading, at any offset.
//
// The reader seeks without asking the bucket anything: [http.ServeContent]
// seeks to the end and back again to find out how long the object is before it
// serves a byte, and a range request that cost two round trips to answer would
// make scrubbing a video a conversation. The bytes are fetched on the first
// read after a seek, as one ranged GET.
func (s *Store) Get(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	// A head rather than a Stat, because the only thing a reader needs is how
	// long the object is. Stat would also fetch the tags for a checksum nothing
	// on this path reads — the ETag a download answers with comes off the
	// rig_file row, not off the bucket — and that would be a second round trip
	// on every download of every file.
	head, err := s.head(ctx, key)
	if err != nil {
		return nil, err
	}

	var size int64
	if head.ContentLength != nil {
		size = *head.ContentLength
	}
	return &object{ctx: ctx, api: s.api, bucket: s.bucket, key: key, size: size}, nil
}

// Stat reports what the bucket knows about the object without opening it.
//
// The checksum comes back from the object's tags, so an object rig did not
// write has an empty one rather than an invented one. Nothing in
// [github.com/simonjanss/rig/files] calls this; it is here because a store that
// could not answer it would be a store a range request had to guess at.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	head, err := s.head(ctx, key)
	if err != nil {
		return blob.Info{}, err
	}

	tags, err := s.tags(ctx, key)
	if err != nil {
		return blob.Info{}, err
	}

	info := blob.Info{Checksum: tags[tagChecksum]}
	if head.ContentLength != nil {
		info.Size = *head.ContentLength
	}
	if head.LastModified != nil {
		info.ModTime = head.LastModified.UTC()
	}
	return info, nil
}

// Delete removes the object and says nothing about a key that was already gone.
//
// S3 answers a delete for a key it does not have with a success, which is the
// behaviour the interface asks for and the reason the sweeper can be a loop
// rather than a state machine.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("rigs3: delete %s: %w", key, err)
	}
	return nil
}

// Mark records on the object what the row already says.
//
// It reads the tag set before writing it, because S3 replaces the whole set and
// the checksum [Store.Put] wrote lives in it. Two calls rather than one, on a
// path that runs once per delete and once per trashed file per sweep — and the
// alternative is a store whose own [blob.Info.Checksum] disappears the first
// time somebody deletes a file.
//
// The mark is a projection of the row and never the other way round. See
// [blob.Marker] for what that costs and why it is the safe direction.
func (s *Store) Mark(ctx context.Context, key string, state blob.State, at time.Time) error {
	tags, err := s.tags(ctx, key)
	if err != nil {
		return err
	}
	tags[tagState] = string(state)
	tags[tagMarkedAt] = at.UTC().Format(time.RFC3339)
	return s.putTags(ctx, key, tags)
}

// SignUpload mints a URL a client can PUT the bytes to itself.
//
// There is no signed download beside it, and that is a decision rather than an
// omission: a signed URL bypasses the endpoint, and the endpoint is where the
// permission check lives. See [blob.Signer].
//
// An object uploaded this way arrives with none of the tags [Store.Put] writes,
// because nothing rig runs saw the bytes. Whatever finalizes such an upload has
// to write them.
func (s *Store) SignUpload(ctx context.Context, key string, expires time.Duration) (string, error) {
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", fmt.Errorf("rigs3: sign an upload of %s: %w", key, err)
	}
	return req.URL, nil
}

// head asks for the object's headers, and reports a missing one the way the
// interface asks: HeadObject has no body to put an error document in, so the
// answer is a bare 404 rather than the NoSuchKey a GET would model.
func (s *Store) head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, s.wrap(key, err)
	}
	return out, nil
}

// tags reads the object's tag set as a map, and reports a missing object the
// way Get and Stat do.
func (s *Store) tags(ctx context.Context, key string) (map[string]string, error) {
	out, err := s.api.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, s.wrap(key, err)
	}

	tags := make(map[string]string, len(out.TagSet))
	for _, t := range out.TagSet {
		if t.Key != nil && t.Value != nil {
			tags[*t.Key] = *t.Value
		}
	}
	return tags, nil
}

// putTags replaces the object's whole tag set, which is the only thing S3
// offers — there is no way to set one tag and leave the rest.
func (s *Store) putTags(ctx context.Context, key string, tags map[string]string) error {
	set := make([]types.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}

	_, err := s.api.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
		Bucket:  aws.String(s.bucket),
		Key:     aws.String(key),
		Tagging: &types.Tagging{TagSet: set},
	})
	if err != nil {
		return s.wrap(key, err)
	}
	return nil
}

// wrap turns the several ways a bucket says "not there" into the one sentinel
// the interface names, and everything else into an error naming the key.
func (s *Store) wrap(key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("rigs3: %s in bucket %s: %w", key, s.bucket, blob.ErrNotFound)
	}
	return fmt.Errorf("rigs3: %s in bucket %s: %w", key, s.bucket, err)
}

// isNotFound covers the three answers that mean the same thing.
//
// GetObject models it as NoSuchKey and HeadObject as NotFound, because a HEAD
// has no body to put an error document in. The status check underneath them is
// not belt and braces: the compatible services do not all return the codes the
// SDK models, and a MinIO 404 that arrived as an ordinary error would become a
// storage failure rather than a missing file.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var response *awshttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}
