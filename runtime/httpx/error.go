package httpx

import (
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

// Answer is [rigerr.Answer], the classification every envelope rig writes shares.
//
// An alias rather than a type of its own: the decision is one implementation and
// naming it twice is how two of them start. It lives in rigerr because it needs
// nothing from net/http, which is what lets a package that only wants to
// classify an error — auth/oauth, answering text/plain — reach it without taking
// on everything this one depends on.
type Answer = rigerr.Answer

// AnswerFor classifies err, and sets on w any header the error carries.
//
// The classification is [rigerr.AnswerFor]'s, including the part a caller must
// not have to remember: **an internal failure's detail never reaches the
// client**. What is added here is the header, and there is one — **a 429 leaves
// with its Retry-After**, because a client told to slow down without being told
// for how long has nothing to do but guess, and clients that guess retry
// immediately.
//
// So this is the call to make from a route that has a [net/http.ResponseWriter]
// in hand, and [rigerr.AnswerFor] the one to make from anywhere that does not.
func AnswerFor(w http.ResponseWriter, err error) Answer {
	if refusal, ok := throttle.RefusalOf(err); ok {
		refusal.Decision().SetHeaders(w.Header())
	}
	return rigerr.AnswerFor(err)
}

// WriteError writes err as an [Error], or writes nothing at all when the caller
// has gone. Pass an empty requestID where there is none to read.
//
// [rigerr.Aborted] is answered with silence because there is nobody to answer.
// The body would go into a socket that is already closed, and the status it set
// would make an abandoned request indistinguishable from a failed one for
// anything reading the response after the fact.
//
// This is not the only copy of that rule, and cannot be: the generated
// DefaultErrorMapper reaches [AnswerFor] directly rather than coming through
// here, so runtime/apibase's own Fail carries it too. Here it covers the routes
// rig mounts itself — the inbox, presence, and the authentication routes — which
// answer through [Fail] and have no generated mapper.
func WriteError(w http.ResponseWriter, requestID string, err error) {
	if rigerr.Aborted(err) {
		return
	}

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
