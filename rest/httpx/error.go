package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/hainuoli/mino/core/errorx"
)

// statusClientClosedRequest is the non-standard status used when the client
// closes the connection.
const statusClientClosedRequest = 499

// errorHandler is the process-wide error mapper configured by SetErrorHandler.
var errorHandler = DefaultErrorHandler

type (
	// StatusCoder is an error carrying an HTTP error status (400 through 599).
	// Its Error text must be suitable for public responses for 4xx statuses.
	// Server errors use a generic message by default. Wrapped errors are supported.
	StatusCoder interface {
		error
		StatusCode() int
	}

	// statusError carries an HTTP status and its public message.
	statusError struct {
		status int
		msg    string
	}

	// requestError carries a request failure status while wrapping the
	// original error for errors.Is and errors.As.
	requestError struct {
		status int
		msg    string
		err    error
	}
)

func (e *statusError) Error() string   { return e.msg }
func (e *statusError) StatusCode() int { return e.status }

func (e *requestError) Error() string { return e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }

// Errorf creates an HTTP status error with a formatted message. The default
// handler exposes 4xx messages and hides 5xx messages. An invalid status is
// treated as an internal error when handled.
func Errorf(status int, format string, args ...any) error {
	return &statusError{status: status, msg: fmt.Sprintf(format, args...)}
}

// SetErrorHandler configures the mapping for every call to Err in the
// process, including calls from different muxes. It is not per-server or
// per-route configuration. A nil handler restores DefaultErrorHandler.
// Configure it at startup, before concurrent calls to Err. Later calls replace
// the previous handler. Do not call it concurrently with Err or another setter
// call. The handler must be safe for concurrent use.
//
// Return DefaultErrorHandler(r, err) to delegate unhandled errors. The status
// must be 400 through 599; a nil body writes only that status.
func SetErrorHandler(h func(*http.Request, error) (int, any)) {
	if h == nil {
		h = DefaultErrorHandler
	}
	errorHandler = h
}

// DefaultErrorHandler maps an error without logging or writing a response.
// Request failures have safe messages, wrapped StatusCoder errors retain their
// status, cancellation maps to 499 with no body, and unknown errors map to 500.
// A nil error returns zero and a nil body.
func DefaultErrorHandler(_ *http.Request, err error) (int, any) {
	var (
		statusErr  StatusCoder
		requestErr *requestError
		tooLarge   *http.MaxBytesError
	)
	status, msg := http.StatusInternalServerError, "internal error"
	switch {
	case err == nil:
		return 0, nil
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest, nil
	case errors.As(err, &requestErr):
		status, msg = requestErr.status, requestErr.msg
	case errors.As(err, &tooLarge):
		status, msg = http.StatusRequestEntityTooLarge, "request body too large"
	case errors.As(err, &statusErr):
		if code := statusErr.StatusCode(); code >= 400 && code <= 599 {
			status = code
			if code < 500 {
				msg = statusErr.Error()
			}
		}
	}
	return status, map[string]string{"message": msg}
}

// requestFailure keeps explicit HTTP errors and cancellation identities intact.
func requestFailure(status int, msg string, err error) error {
	if err == nil || errorx.In(err, context.Canceled, context.DeadlineExceeded) {
		return err
	}
	var requestErr *requestError
	var statusErr StatusCoder
	if errors.As(err, &requestErr) || errors.As(err, &statusErr) {
		return err
	}
	return &requestError{status: status, msg: msg, err: err}
}

// bodyFailure classifies recognizable read failures and preserves other I/O
// errors for the application. It never classifies by matching error text.
func bodyFailure(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return requestFailure(http.StatusRequestEntityTooLarge, "request body too large", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return requestFailure(http.StatusBadRequest, "invalid request body", err)
	}
	return err
}
