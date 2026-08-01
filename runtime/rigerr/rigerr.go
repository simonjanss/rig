// Package rigerr is the error vocabulary shared by generated code.
//
// Every failure carries a machine-readable code as well as a message. Status
// codes alone are too coarse — three unrelated failures all return 400 — and a
// message alone leaves a client parsing prose to decide whether to retry.
package rigerr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the machine-readable reason a request failed. The set is closed: a
// handler cannot invent a status a client has not been told about.
type Code string

const (
	CodeBadRequest          Code = "BadRequest"
	CodeUnauthorized        Code = "Unauthorized"
	CodeForbidden           Code = "Forbidden"
	CodeNotFound            Code = "NotFound"
	CodeConflict            Code = "Conflict"
	CodeUnprocessableEntity Code = "UnprocessableEntity"
	CodeRateLimited         Code = "RateLimited"
	CodeInternal            Code = "Internal"
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

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

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

// Internal reports a server-side failure, keeping the cause for the logs.
func Internal(err error, format string, args ...any) *Error {
	return &Error{Code: CodeInternal, Message: fmt.Sprintf(format, args...), Err: err}
}

// Wrap attaches a cause to an error built by one of the constructors above.
func (e *Error) Wrap(err error) *Error {
	e.Err = err
	return e
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
	return CodeInternal
}

// StatusOf returns the HTTP status for an error.
func StatusOf(err error) int { return CodeOf(err).HTTPStatus() }

// Is reports whether an error carries the given code.
func Is(err error, code Code) bool { return CodeOf(err) == code }
