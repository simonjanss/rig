package rigclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
)

// Op is one call, as the generated method describes it.
//
// The path is relative to the API's base path and already has its parameters
// substituted: escaping an identifier into a route is the generated method's
// job, because only it knows which argument goes where.
type Op struct {
	// Name is the operation this call is, as the document named it —
	// "listTodos", "createTodo". The generated method fills it in.
	//
	// It is what a call's span is called, which is why it is a field rather
	// than something derived from Method and Path: the path here already has
	// its identifiers substituted, so a name built from it would be a new name
	// for every row anybody ever fetched.
	Name string

	Method string
	Path   string
	Query  url.Values
	// Body is encoded as JSON when it is not nil. A pointer to a struct and the
	// struct itself both work; nil means no body at all, which is not the same
	// as an empty object.
	Body any

	// Multipart is a form body, sent instead of Body.
	//
	// The two are exclusive rather than ordered: an Op carrying both is a
	// generated method with a bug in it, and it is reported rather than resolved
	// by a precedence somebody would have to look up.
	//
	// A form is streamed, so it is sent chunked and carries no Content-Length.
	// Computing the envelope's length up front is possible and would mean
	// buffering nothing useful for a header almost nothing needs; an
	// intermediary that insists on one is a deployment problem, and buffering
	// every upload to satisfy it is not a trade this makes.
	Multipart *Multipart

	// Accept is the media type this call will take back. Empty means
	// application/json, which is every endpoint but a download: a download
	// answers with whatever the file turned out to be, and a client that
	// insisted on JSON would be asking for the one thing it is not.
	Accept string

	// Fallback is the path to POST to when Method is QUERY and something between
	// here and the server refuses it — the `_search` alias the router mounts
	// beside the QUERY route. Empty means there is none, and a refusal is
	// reported rather than worked around.
	Fallback string

	// Root says the path is relative to the server rather than to the API's base
	// path. The authentication endpoints are: /auth/login is mounted beside
	// /api/v1 and not inside it, because a sign-in is not a version of the
	// application's API.
	Root bool
}

// Do performs a call and decodes the response body.
//
// The result is a pointer so that a 204 — which several endpoints can answer
// with even when they usually have a body — comes back as nil rather than as a
// zero value indistinguishable from an empty one.
func Do[T any](ctx context.Context, rt *Runtime, op Op, opts ...CallOption) (*T, error) {
	res, err := rt.do(ctx, op, opts)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	var out T
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("rigclient: reading the response to %s %s: %w",
			op.Method, op.Path, err)
	}
	return &out, nil
}

// DoNoContent performs a call that answers with nothing, such as a delete.
//
// Any body is drained and discarded rather than parsed: an endpoint that grows
// one later should not break a client that never wanted it.
func DoNoContent(ctx context.Context, rt *Runtime, op Op, opts ...CallOption) error {
	res, err := rt.do(ctx, op, opts)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

// do performs one call, inside a span when the caller asked for one.
//
// The span is around the whole call rather than around each attempt, and that
// is the difference between a trace saying "listTodos took 400ms, and here is
// the QUERY that was refused inside it" and one showing three unrelated
// requests. The attempts are spans of their own, underneath. See
// [Config.Trace].
func (rt *Runtime) do(ctx context.Context, op Op, opts []CallOption) (*http.Response, error) {
	if rt.trace == nil {
		return rt.call(ctx, op, opts)
	}

	var res *http.Response
	err := rt.trace(ctx, op.spanName(), func(ctx context.Context) error {
		var err error
		//nolint:bodyclose // the response is returned below; Do and DoNoContent close it
		res, err = rt.call(ctx, op, opts)
		return err
	})
	return res, err
}

// spanName is what one call is called in a trace.
//
// An Op with no name — one somebody assembled by hand — falls back to its
// method alone. Boring, and bounded: the path is not usable here, because by
// this point it has an identifier in it.
func (op Op) spanName() string {
	if op.Name == "" {
		return op.Method
	}
	return op.Name
}

// call sends the request, handling the two things every call shares: a
// credential that may need refreshing, and a QUERY an intermediary may refuse.
//
// It returns a response whose body is still open and whose status is a success.
func (rt *Runtime) call(ctx context.Context, op Op, opts []CallOption) (*http.Response, error) {
	call := newCall(opts)

	if op.Method == MethodQuery && op.Fallback != "" && rt.searchesByPost() {
		op = op.asPost()
	}

	// Where the form's bodies start, taken before anything reads them. See
	// [Multipart.mark].
	marks := op.Multipart.mark()

	res, err := rt.send(ctx, op, call)
	if err != nil {
		return nil, err
	}

	// A method nobody in the chain recognizes is answered 405 by a proxy or 501
	// by a server that has never heard of it. Both mean the same thing to a
	// client: use the alias, and stop asking.
	if op.Method == MethodQuery && op.Fallback != "" &&
		(res.StatusCode == http.StatusMethodNotAllowed || res.StatusCode == http.StatusNotImplemented) {
		drain(res)
		rt.rememberSearchByPost()

		res, err = rt.send(ctx, op.asPost(), call)
		if err != nil {
			return nil, err
		}
	}

	// One retry, and only for a credential that can do something about it. A
	// blind retry on 401 is a way to lock an account out with a wrong password.
	if res.StatusCode == http.StatusUnauthorized {
		if re, ok := rt.Credential().(Reauthorizer); ok && !call.retried {
			// Before the refresh, not after: an upload the client cannot send
			// again is a call that has to come back to the caller, and doing it
			// here means it comes back before a token was spent on it. The 401
			// travels with it, so the failure still answers IsUnauthorized.
			if err := op.Multipart.rewind(marks); err != nil {
				defer res.Body.Close()
				return nil, errors.Join(readError(res), err)
			}

			drain(res)
			done, err := re.Reauthorize(ctx)
			if err != nil {
				return nil, err
			}
			if done {
				call.retried = true
				res, err = rt.send(ctx, op, call)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	// A 304 is the answer to a question only a conditional call asked, so it is
	// a success for that caller and an unexplained failure for anybody else.
	// Which is why it is gated on the option rather than on the status alone.
	if res.StatusCode == http.StatusNotModified && call.conditional {
		return res, nil
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		return nil, readError(res)
	}
	return res, nil
}

// send performs one attempt, inside a span of its own when the caller traces.
//
// Named by the method actually used, so the QUERY that was refused and the POST
// that replaced it are two lines in a trace rather than one repeated.
func (rt *Runtime) send(ctx context.Context, op Op, call *call) (*http.Response, error) {
	if rt.trace == nil {
		return rt.attempt(ctx, op, call)
	}

	var res *http.Response
	err := rt.trace(ctx, "send "+op.Method, func(ctx context.Context) error {
		var err error
		//nolint:bodyclose // the response is returned below; do decides what happens to it
		res, err = rt.attempt(ctx, op, call)
		return err
	})
	return res, err
}

// attempt builds and performs one HTTP request.
func (rt *Runtime) attempt(ctx context.Context, op Op, call *call) (*http.Response, error) {
	if op.Body != nil && op.Multipart != nil {
		return nil, fmt.Errorf("rigclient: %s %s carries both a JSON body and a form; "+
			"a generated method sends one or the other", op.Method, op.Path)
	}

	var (
		body        io.Reader
		contentType string
	)
	switch {
	case op.Multipart != nil:
		body, contentType = op.Multipart.stream()
	case op.Body != nil:
		encoded, err := json.Marshal(op.Body)
		if err != nil {
			return nil, fmt.Errorf("rigclient: encoding the body of %s %s: %w",
				op.Method, op.Path, err)
		}
		body, contentType = bytes.NewReader(encoded), "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, rt.url(op, call.query), body)
	if err != nil {
		// Do closes the body; nothing else does, and a form's writer is a
		// goroutine blocked on a pipe nobody will read.
		if c, ok := body.(io.Closer); ok {
			c.Close()
		}
		return nil, err
	}

	accept := op.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", rt.userAgent)
	if rt.revision != "" {
		req.Header.Set(rt.revisionHeader, rt.revision)
	}
	if rt.requestID != nil {
		if id := rt.requestID(); id != "" {
			req.Header.Set(rt.requestIDHeader, id)
		}
	}
	for key, values := range rt.header {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	for key, values := range call.header {
		req.Header.Del(key)
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}

	if cred := rt.Credential(); cred != nil && !call.anonymous {
		if err := cred.Apply(ctx, req); err != nil {
			return nil, err
		}
	}

	return call.client(rt.http).Do(req)
}

// url builds the absolute URL for an operation.
//
// extra is what a [CallOption] added, and it wins where the two name the same
// parameter: the operation's own query came from the typed arguments of a
// generated method, and an option is the caller saying something about this one
// call afterwards.
func (rt *Runtime) url(op Op, extra url.Values) string {
	base := rt.api.BasePath
	if op.Root {
		base = ""
	}

	u := *rt.base
	u.Path = strings.TrimRight(u.Path, "/") + base + op.Path

	if len(extra) > 0 {
		merged := url.Values{}
		maps.Copy(merged, op.Query)
		maps.Copy(merged, extra)
		u.RawQuery = merged.Encode()
		return u.String()
	}

	if len(op.Query) > 0 {
		u.RawQuery = op.Query.Encode()
	}
	return u.String()
}

// MethodQuery is the HTTP method a generated search uses. It is a method rather
// than a POST because a search is a read: it has a body, and it is safe and
// idempotent, and pretending otherwise is what made every API in the world
// invent /_search.
const MethodQuery = "QUERY"

// asPost is the same operation addressed to the alias route.
func (op Op) asPost() Op {
	op.Method = http.MethodPost
	op.Path = op.Fallback
	op.Fallback = ""
	return op
}

func (rt *Runtime) searchesByPost() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	return rt.searchByPost
}

func (rt *Runtime) rememberSearchByPost() {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	rt.searchByPost = true
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
}
