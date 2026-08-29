package rigclient

import (
	"context"
	"io"
	"mime"
	"net/http"
)

// Content is a response whose body is bytes rather than JSON.
//
// Nothing here reads ahead. A download is streamed, so a file larger than
// memory can go to disk without passing through it — which is why this is not a
// []byte, and why Body is the caller's to close. Until it is closed the
// connection is not returned to the pool.
type Content struct {
	// Body is the bytes. Close it.
	Body io.ReadCloser

	// ContentType is what the server says the bytes are: the type it sniffed
	// when the file was uploaded, never the one the client claimed when it sent
	// it.
	ContentType string

	// Length is the number of bytes in Body, or -1 when the server did not say.
	// On the 206 that [WithRange] asks for it is the length of the range and
	// not of the file.
	Length int64

	// Filename is the name from Content-Disposition, or empty. It is a
	// suggestion and not a path: a program writing it to disk decides where, and
	// sanitizes what.
	Filename string

	// ETag identifies this version of the file, for a later call to quote back
	// through [WithIfNoneMatch].
	ETag string

	// Status is 200 for the whole file, 206 for the range [WithRange] asked
	// for, and 304 for a body the caller already has — in which case Body is
	// empty and closing it is all there is to do with it.
	Status int
}

// DoContent performs a call whose response is bytes.
//
// The returned Content owns an open connection, so a caller that does not close
// its Body leaks one. It is the one thing a generated download method cannot do
// for you: the whole point of streaming is that the bytes outlive the call.
//
// A 304 comes back as a Content rather than as an error, because a caller who
// asked with [WithIfNoneMatch] is owed the answer they asked for. A 204 comes
// back as nil, nil, the same as [Do].
func DoContent(ctx context.Context, rt *Runtime, op Op, opts ...CallOption) (*Content, error) {
	res, err := rt.do(ctx, op, opts)
	if err != nil {
		return nil, err
	}

	if res.StatusCode == http.StatusNoContent {
		drain(res)
		return nil, nil
	}
	return readContent(res), nil
}

// readContent describes a response without reading it.
func readContent(res *http.Response) *Content {
	c := &Content{
		Body:        res.Body,
		ContentType: res.Header.Get("Content-Type"),
		Length:      res.ContentLength,
		ETag:        res.Header.Get("ETag"),
		Status:      res.StatusCode,
	}

	// ParseMediaType decodes an RFC 5987 filename* into the filename parameter,
	// which is how a non-ASCII name survives the trip.
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			c.Filename = params["filename"]
		}
	}
	return c
}
