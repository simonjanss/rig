package rigclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	Method string
	Path   string
	Query  url.Values
	// Body is encoded as JSON when it is not nil. A pointer to a struct and the
	// struct itself both work; nil means no body at all, which is not the same
	// as an empty object.
	Body any

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

// do sends the request, handling the two things every call shares: a credential
// that may need refreshing, and a QUERY an intermediary may refuse.
//
// It returns a response whose body is still open and whose status is a success.
func (rt *Runtime) do(ctx context.Context, op Op, opts []CallOption) (*http.Response, error) {
	call := newCall(opts)

	if op.Method == MethodQuery && op.Fallback != "" && rt.searchesByPost() {
		op = op.asPost()
	}

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

	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer res.Body.Close()
		return nil, readError(res)
	}
	return res, nil
}

// send builds and performs one HTTP request.
func (rt *Runtime) send(ctx context.Context, op Op, call *call) (*http.Response, error) {
	var body io.Reader
	if op.Body != nil {
		encoded, err := json.Marshal(op.Body)
		if err != nil {
			return nil, fmt.Errorf("rigclient: encoding the body of %s %s: %w",
				op.Method, op.Path, err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, rt.url(op), body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	if op.Body != nil {
		req.Header.Set("Content-Type", "application/json")
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

	return rt.http.Do(req)
}

// url builds the absolute URL for an operation.
func (rt *Runtime) url(op Op) string {
	base := rt.api.BasePath
	if op.Root {
		base = ""
	}

	u := *rt.base
	u.Path = strings.TrimRight(u.Path, "/") + base + op.Path
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
