package httpx

import (
	"errors"
	"net/http"

	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// Error is the envelope every route rig owns answers a failure with.
//
// Flat — `code` and `message` at the top level, not nested under an `error` key —
// because that is what the generated server ships and what both of rig's client
// libraries parse. A nested envelope decodes into an all-zero struct there: the
// code comes out empty, so every `IsNotFound` and `IsForbidden` answers false and
// the caller is left with the status and nothing else.
type Error struct {
	// Code is the machine-readable one. A client branches on this, never on the
	// message.
	Code rigerr.Code `json:"code"`
	// Message is for a person. For an internal failure it says nothing about the
	// internals — see [AnswerFor].
	Message string `json:"message"`
	// RequestID is what finds the detail in the logs, and for an internal failure
	// it is the only thing a client is told. Omitted where there is none: the
	// hand-written routes have no request context to read one from.
	RequestID string `json:"requestId,omitempty"`
	// Fields is one member per field of the input, for a validation failure. It
	// is the difference between a client highlighting the field somebody got
	// wrong and a client parsing prose to work out which one that was.
	Fields any `json:"fields,omitempty"`
}

// Answer is what an error means on the wire, before anything is encoded.
//
// Separate from [Error] so a caller with an envelope of its own — the generated
// server, whose field names go through `api.json_case` — can share the decision
// without sharing the shape. That is the whole seam between the two: the
// classification is one implementation, the encoding is two.
type Answer struct {
	// Code and Status are the same fact twice, because a caller assembling a
	// response wants both and deriving one from the other at four call sites is
	// how they drift.
	Code   rigerr.Code
	Status int
	// Message is already redacted.
	Message string
	// Fields is nil unless the failure carried per-field detail.
	Fields any
}

// AnswerFor classifies err, and sets on w any header the error carries.
//
// Two things it does that a caller must not have to remember. **An internal
// failure's detail never reaches the client**: it is exactly the kind of thing
// that leaks a table name, a constraint, or a connection string, so the message
// becomes a fixed sentence and the request id becomes the only way to find out
// more. And **a 429 leaves with its Retry-After**, because a client told to slow
// down without being told for how long has nothing to do but guess, and clients
// that guess retry immediately.
func AnswerFor(w http.ResponseWriter, err error) Answer {
	code := rigerr.CodeOf(err)

	message := err.Error()
	var typed *rigerr.Error
	if errors.As(err, &typed) {
		message = typed.Message
	}
	if code == rigerr.CodeInternal {
		message = "something went wrong"
	}

	if refusal, ok := throttle.RefusalOf(err); ok {
		refusal.Decision().SetHeaders(w.Header())
	}

	fields, _ := rigerr.FieldsOf(err)

	return Answer{Code: code, Status: code.HTTPStatus(), Message: message, Fields: fields}
}

// WriteError writes err as an [Error]. Pass an empty requestID where there is
// none to read.
func WriteError(w http.ResponseWriter, requestID string, err error) {
	answer := AnswerFor(w, err)
	WriteJSON(w, answer.Status, Error{
		Code:      answer.Code,
		Message:   answer.Message,
		RequestID: requestID,
		Fields:    answer.Fields,
	})
}

// Fail is [WriteError] in the shape a route's error-writer option takes.
//
// It is the fallback for a handler mounted on its own. A project with a generated
// server passes that server's writer instead, so a failure from one of these
// routes carries the request id and lands in the same log line as every other
// route's.
func Fail(w http.ResponseWriter, _ *http.Request, err error) {
	WriteError(w, "", err)
}
