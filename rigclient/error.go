package rigclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Error is a request the server refused.
//
// The code, not the status, is what to switch on. Three unrelated failures share
// a 400 and none of them share a code, which is the whole reason the generated
// server sends one.
type Error struct {
	// Status is the HTTP status, for the cases where only it is meaningful — a
	// 502 from something in front of the server, say, which carries no code.
	Status int
	// Code is the machine-readable reason. Empty when the failure came from
	// something that is not a rig server.
	Code rigerr.Code
	// Message is prose, for a person. It is not meant to be parsed.
	Message string
	// RequestID correlates this failure with the server's logs. Quoting it in a
	// bug report is the difference between a search and a guess.
	RequestID string
	// Fields is present when the failure was validation, and is shaped like the
	// request body that failed — one member per field, holding what was wrong
	// with it.
	//
	// These are the bytes. A call made through a generated method comes back as
	// that method's own error — client.TodoCreateError, which is a [Failure] —
	// and has them decoded already. [FieldsAs] is for a call made by hand.
	Fields json.RawMessage
	// RetryAfter is how long the server asked the caller to wait, from the
	// header of the same name. Zero when it said nothing.
	RetryAfter time.Duration
	// Body is the raw response, kept for a failure that decoded into nothing
	// useful. Bounded: a proxy's HTML error page is not worth megabytes.
	Body string
}

// Error is the code and the message, prefixed with "rig: " so that a failure
// from the server is distinguishable at a glance from one the transport had.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("rig: ")
	if e.Code != "" {
		b.WriteString(string(e.Code))
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString(http.StatusText(e.Status))
	}
	fmt.Fprintf(&b, " (%d)", e.Status)
	if e.RequestID != "" {
		b.WriteString(" [request ")
		b.WriteString(e.RequestID)
		b.WriteString("]")
	}
	return b.String()
}

// Refusal is [Error] under a second name, for embedding.
//
// A struct that embedded *Error would get a field called Error, and a field at
// depth zero shadows the promoted method of the same name — so the struct would
// carry a refusal without being one. The alias is the whole of the fix: the
// field is called Refusal, Error stays a method, and there is still one error
// type rather than two that have to be kept in step.
type Refusal = Error

// Failure is everything a refused call said, in one value: the envelope, and
// the per-field detail decoded into the shape of the body that went out.
//
// F is that shape, and a caller does not name it — the generated client declares
// a function per call that does, so client.TodoCreateError(err) reads back what
// Todos.Create refused. Naming it by hand is what [FieldsAs] asks for, and it is
// why the per-call function exists: the wrong shape decodes perfectly and answers
// with an empty struct, because every member of a field-error shape is optional.
//
// The envelope is embedded, so Code, Message, RequestID, Status and RetryAfter
// read exactly as they do on [Error].
type Failure[F any] struct {
	*Refusal

	// Fields is what was wrong with each member of the body, and is nil when the
	// refusal was not about the body — which is every refusal but a 422. A 404
	// carries a code and a message and nothing to attach to a control, and a
	// zero-valued shape there would look like a body nobody complained about.
	//
	// It shadows [Error.Fields], which is the point: the typed value is what a
	// caller wants, and the bytes are still under Refusal for anything that
	// wants those.
	Fields *F
}

// Unwrap returns the refusal, so errors.As reaches [Error] and [CodeOf],
// [FieldsAs] and the Is predicates all answer about a Failure unchanged.
func (f *Failure[F]) Unwrap() error { return f.Refusal }

// As reads a refusal back as the shape of the body that caused it.
//
// It reports false for anything that is not a refusal — a request that never
// reached the server carries no envelope, so there is nothing for a code or a
// field to have come from — which is what makes
//
//	if refused, ok := client.TodoCreateError(err); ok {
//
// the whole of the question. The generated per-call functions are one line each
// on top of this; reach for it directly only for a call made by hand, where
// nothing chose a shape for you.
func As[F any](err error) (*Failure[F], bool) {
	var e *Error
	if err == nil || !errors.As(err, &e) {
		return nil, false
	}

	f := &Failure[F]{Refusal: e}
	if len(e.Fields) > 0 {
		// A body that does not fit the shape leaves Fields nil and the raw bytes
		// where they were. Skew between a client and a server is not a reason to
		// lose the code and the message as well, which is what reporting the
		// decode failure instead would do.
		var fields F
		if json.Unmarshal(e.Fields, &fields) == nil {
			f.Fields = &fields
		}
	}
	return f, true
}

// FieldsAs decodes the per-field failures into the generated shape for the input
// that failed — TodoCreateFields, say, whose members are the input's members.
//
// It reports false when the failure carried none, so the ordinary
//
//	if fields, ok := rigclient.FieldsAs[client.TodoCreateFields](err); ok {
//
// reads as the question it is.
//
// It answers the fields alone. The generated client declares a function per call
// — client.TodoCreateError(err) — that answers the envelope with them, and picks
// the shape for you: this one cannot tell that the shape it was handed is not the
// one that failed, because every member of one is optional.
func FieldsAs[T any](err error) (T, bool) {
	var out T

	var e *Error
	if !errors.As(err, &e) || len(e.Fields) == 0 {
		return out, false
	}
	if err := json.Unmarshal(e.Fields, &out); err != nil {
		return out, false
	}
	return out, true
}

// CodeOf returns the code an error carries, or empty when it carries none.
func CodeOf(err error) rigerr.Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsNotFound reports whether the server answered that there is no such thing.
// A row in another tenant is the same answer, deliberately.
func IsNotFound(err error) bool { return CodeOf(err) == rigerr.CodeNotFound }

// IsConflict reports whether the request contradicted the current state.
func IsConflict(err error) bool { return CodeOf(err) == rigerr.CodeConflict }

// IsUnauthorized reports whether no valid credential was presented.
func IsUnauthorized(err error) bool { return CodeOf(err) == rigerr.CodeUnauthorized }

// IsForbidden reports whether the caller is known but not permitted.
func IsForbidden(err error) bool { return CodeOf(err) == rigerr.CodeForbidden }

// IsInvalid reports whether a well-formed request failed validation, which is
// the failure [FieldsAs] has something to say about.
func IsInvalid(err error) bool { return CodeOf(err) == rigerr.CodeUnprocessableEntity }

// IsRateLimited reports whether the caller was asked to slow down.
func IsRateLimited(err error) bool { return CodeOf(err) == rigerr.CodeRateLimited }

// IsTooLarge reports whether the body was larger than the endpoint accepts.
func IsTooLarge(err error) bool { return CodeOf(err) == rigerr.CodeTooLarge }

// IsUnsupportedMediaType reports whether the body's content type was refused.
func IsUnsupportedMediaType(err error) bool {
	return CodeOf(err) == rigerr.CodeUnsupportedMediaType
}

// IsUpgradeRequired reports whether this client is older than the server will
// serve, and has to be regenerated against the current schema.
//
// It is the one failure here that nothing the caller does at runtime will fix —
// not a retry, not a different input, not signing in again — so it is usually
// the one worth turning into a message a person sees rather than an error a
// handler swallows.
func IsUpgradeRequired(err error) bool { return CodeOf(err) == rigerr.CodeUpgradeRequired }

// maxErrorBody bounds what is kept from a failure nobody can parse. Enough to
// recognize a gateway's error page, not enough to be a memory problem.
const maxErrorBody = 8 << 10

// errorBody is the envelope the generated server sends on every failure.
type errorBody struct {
	Code      rigerr.Code     `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"requestId"`
	Fields    json.RawMessage `json:"fields"`
}

// readError turns a refused response into an error.
//
// Anything that does not decode is still an error with a status: a 502 from a
// load balancer is not JSON and is not the server's fault, and a client that
// panicked on it would be reporting the wrong bug.
func readError(res *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))

	e := &Error{Status: res.StatusCode, Body: string(raw)}
	if after := res.Header.Get("Retry-After"); after != "" {
		if seconds, err := strconv.Atoi(after); err == nil {
			e.RetryAfter = time.Duration(seconds) * time.Second
		}
	}

	var decoded errorBody
	if err := json.Unmarshal(raw, &decoded); err == nil {
		e.Code, e.Message, e.RequestID, e.Fields =
			decoded.Code, decoded.Message, decoded.RequestID, decoded.Fields
	}
	if e.RequestID == "" {
		e.RequestID = res.Header.Get(DefaultRequestIDHeader)
	}
	return e
}
