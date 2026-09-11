// Package rigerr is the error vocabulary shared by generated code.
//
// Every failure carries a machine-readable code as well as a message. Status
// codes alone are too coarse — three unrelated failures all return 400 — and a
// message alone leaves a client parsing prose to decide whether to retry.
package rigerr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Code is the machine-readable reason a request failed. The set is closed: a
// handler cannot invent a status a client has not been told about.
type Code string

// The codes. Each one maps to a status through [Code.HTTPStatus], and adding
// one here without a case there is what the default arm is for — an unmapped
// code is a 500 rather than a compile error, so the two are kept in step by
// reading them together.
const (
	CodeBadRequest          Code = "BadRequest"
	CodeUnauthorized        Code = "Unauthorized"
	CodeForbidden           Code = "Forbidden"
	CodeNotFound            Code = "NotFound"
	CodeConflict            Code = "Conflict"
	CodeUnprocessableEntity Code = "UnprocessableEntity"
	CodeRateLimited         Code = "RateLimited"
	// CodeTooLarge reports a request body past the limit the endpoint accepts.
	//
	// It is separate from a bad request because the caller's request was
	// well-formed and the answer is a number: send fewer bytes. A 400 that also
	// means a malformed body cannot say that.
	CodeTooLarge Code = "TooLarge"
	// CodeUnsupportedMediaType reports a body whose content type the endpoint
	// does not accept — a JSON-only endpoint sent a form, or an upload of a type
	// the project does not allow.
	CodeUnsupportedMediaType Code = "UnsupportedMediaType"
	// CodeUpgradeRequired reports that the caller was built against an API
	// revision the server no longer serves.
	//
	// It is its own code rather than a bad request because it is the one failure
	// a client can actually do something about, and "regenerate your client" is
	// not advice anybody can take from a 400 that also means a malformed body.
	CodeUpgradeRequired Code = "UpgradeRequired"
	// CodeUnavailable reports that something the server was waiting on did not
	// answer in time.
	//
	// It is separate from an internal failure because nothing here is broken: the
	// path works and was too slow, which is the difference between a caller that
	// should retry and one that should not. A 500 tells it neither. It is what a
	// [context.DeadlineExceeded] becomes, and nothing in rig's own request path
	// sets a deadline on a request context — so one arriving here was set below
	// the handler, by a pool acquire, an upstream client, or the application's own
	// timeout around a query.
	CodeUnavailable Code = "Unavailable"
	CodeInternal    Code = "Internal"
)

// HTTPStatus maps a code to its status.
func (c Code) HTTPStatus() int {
	switch c {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeUnprocessableEntity:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeUnsupportedMediaType:
		return http.StatusUnsupportedMediaType
	case CodeUpgradeRequired:
		return http.StatusUpgradeRequired
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Error is a failure with a code.
type Error struct {
	Code    Code
	Message string
	// Err is the underlying cause, kept for logs and errors.Is. It is never
	// shown to a client: an internal failure's detail is exactly the kind of
	// thing that leaks a table name or a connection string.
	Err error
}

// Error is the code, the message, and the cause — everything, because this is
// for a log. What a client sees is assembled from [Error.Code] and
// [Error.Message] alone.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause, so errors.Is and errors.As reach it.
func (e *Error) Unwrap() error { return e.Err }

// HTTPStatus is the status this error should be returned as.
func (e *Error) HTTPStatus() int { return e.Code.HTTPStatus() }

func newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// BadRequest reports a malformed request or an unparseable parameter.
func BadRequest(format string, args ...any) *Error { return newf(CodeBadRequest, format, args...) }

// Unauthorized reports that no valid session or key was presented.
func Unauthorized(format string, args ...any) *Error { return newf(CodeUnauthorized, format, args...) }

// Forbidden reports that the caller is known but not permitted.
func Forbidden(format string, args ...any) *Error { return newf(CodeForbidden, format, args...) }

// NotFound reports that no such resource exists, or that it belongs to another
// tenant. The two are deliberately indistinguishable: answering 403 for a row
// in someone else's tenant confirms the row exists.
func NotFound(format string, args ...any) *Error { return newf(CodeNotFound, format, args...) }

// Conflict reports that the request contradicts the current state.
func Conflict(format string, args ...any) *Error { return newf(CodeConflict, format, args...) }

// Invalid reports a well-formed request that failed validation.
func Invalid(format string, args ...any) *Error {
	return newf(CodeUnprocessableEntity, format, args...)
}

// RateLimited reports that the caller should slow down.
func RateLimited(format string, args ...any) *Error { return newf(CodeRateLimited, format, args...) }

// TooLarge reports a body past the limit the endpoint accepts.
func TooLarge(format string, args ...any) *Error { return newf(CodeTooLarge, format, args...) }

// UnsupportedMediaType reports a body whose content type the endpoint refuses.
func UnsupportedMediaType(format string, args ...any) *Error {
	return newf(CodeUnsupportedMediaType, format, args...)
}

// UpgradeRequired reports that the caller is older than the server will serve.
func UpgradeRequired(format string, args ...any) *Error {
	return newf(CodeUpgradeRequired, format, args...)
}

// Unavailable reports that something the server depends on did not answer in
// time, keeping the cause for the logs.
func Unavailable(err error, format string, args ...any) *Error {
	return &Error{Code: CodeUnavailable, Message: fmt.Sprintf(format, args...), Err: err}
}

// Internal reports a server-side failure, keeping the cause for the logs.
func Internal(err error, format string, args ...any) *Error {
	return &Error{Code: CodeInternal, Message: fmt.Sprintf(format, args...), Err: err}
}

// Wrap attaches a cause to an error built by one of the constructors above.
func (e *Error) Wrap(err error) *Error {
	e.Err = err
	return e
}

// FieldCode says what kind of thing was wrong with one field, for a client
// that has to decide something rather than display something.
//
// The set is fixed and shared by every project, so a client switches on it
// once. What varies goes in the message.
type FieldCode string

// The kinds of wrong a field can be.
const (
	// FieldCodeCannotBeEmpty means the field is required and was left blank.
	FieldCodeCannotBeEmpty FieldCode = "CannotBeEmpty"
	// FieldCodeCannotBeNull means the field was cleared and the column cannot
	// hold null.
	FieldCodeCannotBeNull FieldCode = "CannotBeNull"
	// FieldCodeTooLong means longer than the column allows.
	FieldCodeTooLong FieldCode = "TooLong"
	// FieldCodeTooShort means shorter than the rule allows.
	FieldCodeTooShort FieldCode = "TooShort"
	// FieldCodeOutOfRange means outside the allowed range.
	FieldCodeOutOfRange FieldCode = "OutOfRange"
	// FieldCodeInvalidValue means not one of the values this field accepts.
	FieldCodeInvalidValue FieldCode = "InvalidValue"
	// FieldCodeAlreadyExists means another row has this value and it has to be
	// unique.
	FieldCodeAlreadyExists FieldCode = "AlreadyExists"
	// FieldCodeNotFound means it names something that does not exist.
	FieldCodeNotFound FieldCode = "NotFound"
	// FieldCodeNotAllowed means a legal value, but not one this caller may
	// set, or not from the state the row is in.
	FieldCodeNotAllowed FieldCode = "NotAllowed"
)

// FieldError is one thing wrong with one field.
//
// It does not name the field. It is reached through the member of an input's
// error struct that stands for that field, so the name is where the value is,
// and there is no second copy of it to disagree.
//
// It lives here rather than being generated per project because none of it is
// per project: the same nine codes and the same two members, so one definition
// serves every model and every client.
type FieldError struct {
	Code    FieldCode `json:"code"`
	Message string    `json:"message"`
}

// Error implements error, so a validation rule can return one directly.
func (e *FieldError) Error() string { return string(e.Code) + ": " + e.Message }

// ErrorCode implements [Coder], and says Internal on purpose.
//
// A field error means nothing on its own: it says what was wrong without
// saying what it was wrong about. Reaching the HTTP layer as the whole answer
// means one was returned from somewhere that has no field to attach it to — a
// service method rather than a validation rule — and the client cannot act on
// "CannotBeEmpty" with nothing to hang it off.
//
// So it is a 500 with the generic message, which is what any other bug is. The
// alternative, a 422 with an empty body, would blame the caller for a mistake
// in the service.
func (e *FieldError) ErrorCode() Code { return CodeInternal }

// NewFieldError builds one. A rule with nothing better to say than "no" uses
// FieldCodeInvalidValue.
func NewFieldError(code FieldCode, format string, args ...any) *FieldError {
	return &FieldError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsFieldError returns the field error an error carries, and whether it was
// one at all.
//
// It is what separates a rule that failed from a rule that could not be run:
// the first is about the caller's input and belongs in a 422 under the field it
// names, the second is about the server and belongs nowhere near it.
//
// Named As rather than Is because it hands back the value, the way [errors.As]
// does.
func AsFieldError(err error) (*FieldError, bool) {
	var field *FieldError
	if errors.As(err, &field) {
		return field, true
	}
	return nil, false
}

// Wrap adds context to an error, keeping whatever code it already carried.
//
// An error with no code becomes Internal, which is the honest answer for one
// that never passed through this package: nobody decided what it means to a
// client, so it is a server problem until somebody does. An error that does
// carry a code keeps it — a rule that refused with a Conflict is still a
// conflict once the layer above has said which rule it was.
func Wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}

	message := fmt.Sprintf(format, args...)

	var coded *Error
	if errors.As(err, &coded) {
		return &Error{Code: coded.Code, Message: message + ": " + coded.Message, Err: err}
	}

	var c Coder
	if errors.As(err, &c) {
		return &Error{Code: c.ErrorCode(), Message: message, Err: err}
	}

	return &Error{Code: CodeInternal, Message: message, Err: err}
}

// Coder is an error that names its own code.
//
// It exists for a failure that is more than a message — the generated
// per-input validation errors implement it — so such an error can be returned
// as itself rather than flattened into an [Error] carrying prose.
type Coder interface {
	error
	ErrorCode() Code
}

// FieldReporter is an error that can say what was wrong with each field.
//
// ErrorFields returns a value shaped like the input it describes: one member
// per field of the request, each holding the problem with that field or
// nothing. A client can then attach every message to the control the person is
// looking at, instead of parsing one sentence for field names.
type FieldReporter interface {
	error
	ErrorFields() any
}

// CodeOf returns the code an error carries, or Internal when it carries none.
//
// An error that never passed through this package is a bug rather than a
// client mistake, and reporting it as anything other than a server error would
// be telling the caller to fix something that is not theirs.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}

	var c Coder
	if errors.As(err, &c) {
		return c.ErrorCode()
	}

	// Last, so that a code somebody stated explicitly still wins over the cause
	// it happens to carry: a handler that wraps a timed-out query in a NotFound
	// meant NotFound.
	if TimedOut(err) {
		return CodeUnavailable
	}
	return CodeInternal
}

// Aborted reports whether err is a caller that went away.
//
// Nothing in rig ever cancels a request context. There is no BaseContext on the
// server it builds, and shutdown waits for the requests in flight rather than
// cancelling them — so a [context.Canceled] arriving at a handler boundary is
// the caller hanging up, and nothing else. A closed browser tab, not a failure.
//
// It deliberately has no [Code]. A status would have to be a non-standard one,
// which would reach the generated OpenAPI document and every client's Is
// helpers, and an abandoned request does not need a status: it needs no
// response, because there is nobody left to read one.
//
// The assumption it rests on is rig's, not Go's. A handler that cancels a
// context of its own and returns that error is telling this function the caller
// left when it did not, and would have its answer dropped.
func Aborted(err error) bool { return errors.Is(err, context.Canceled) }

// TimedOut reports whether err is something the server waited on that did not
// answer.
//
// The mirror image of [Aborted] rather than a sibling of it, and the reason the
// two are not one predicate. Nothing in rig's request path puts a deadline on a
// request context either — the read and write timeouts on the server are
// connection deadlines and surface as write errors — so a
// [context.DeadlineExceeded] here was set below the handler by something the
// server was waiting on. Nobody hung up; something did not answer.
//
// Unlike [Aborted] this does carry a [Code]: [CodeUnavailable], through
// [CodeOf]. There is a caller waiting for an answer, and 503 is a more useful
// one than 500 to a client deciding whether to retry.
func TimedOut(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

// FieldsOf returns the per-field detail an error carries, if it carries any.
func FieldsOf(err error) (any, bool) {
	var r FieldReporter
	if errors.As(err, &r) {
		return r.ErrorFields(), true
	}
	return nil, false
}

// StatusOf returns the HTTP status for an error.
func StatusOf(err error) int { return CodeOf(err).HTTPStatus() }

// Is reports whether an error carries the given code.
func Is(err error, code Code) bool { return CodeOf(err) == code }
