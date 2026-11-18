package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/hainuoli/mino/core/errorx"
	"github.com/hainuoli/mino/core/mapping"
)

// statusClientClosedRequest is the non-standard status used when the client
// closes the connection.
const statusClientClosedRequest = 499

// errorHandler is the process-wide error mapper configured by SetErrorHandler.
var errorHandler = DefaultErrorHandler

type (
	// StatusCoder is an error carrying an HTTP status (400-599).
	// Error text must be client-safe: the default handler exposes 4xx
	// messages and hides 5xx ones. Wrapped errors are supported.
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
// handler exposes 4xx messages and hides 5xx ones; invalid statuses map to
// 500 when handled.
//
//	err := httpx.Errorf(http.StatusBadRequest, "invalid page %d", 9)
//	// err.Error() -> "invalid page 9"
func Errorf(status int, format string, args ...any) error {
	return &statusError{status: status, msg: fmt.Sprintf(format, args...)}
}

// SetErrorHandler configures the process-wide mapping used by Err across all
// muxes; it is not per-server configuration. A nil handler restores
// DefaultErrorHandler, and later calls replace earlier ones. Call it once at
// startup, never concurrently with Err, and make the handler safe for
// concurrent use.
//
// The status must be 400-599 and a nil body writes only the status. Return
// DefaultErrorHandler(r, err) to delegate unhandled errors.
//
//	httpx.SetErrorHandler(func(r *http.Request, err error) (int, any) {
//		if errors.Is(err, ErrNotFound) {
//			return http.StatusNotFound, map[string]string{"message": "not found"}
//		}
//		return httpx.DefaultErrorHandler(r, err)
//	})
func SetErrorHandler(h func(*http.Request, error) (int, any)) {
	if h == nil {
		h = DefaultErrorHandler
	}
	errorHandler = h
}

// DefaultErrorHandler maps an error to a status and body without logging or
// writing a response. Request failures keep safe messages, StatusCoder errors
// keep their status, cancellation maps to 499 with no body, and unknown errors
// map to 500 with a generic message. A nil error returns zero and a nil body.
//
//	status, body := httpx.DefaultErrorHandler(nil, errors.New("boom"))
//	// status -> 500, body -> {"message":"internal error"}
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

// wrapRequestError adds a status and public message to err while preserving
// explicit HTTP errors and cancellation identities.
func wrapRequestError(status int, msg string, err error) error {
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

// wrapBindingError preserves invalid destinations as programming errors and
// classifies conversion failures as bad request parameters.
func wrapBindingError(err error) error {
	if errors.Is(err, mapping.ErrInvalidTarget) {
		return err
	}
	return wrapRequestError(http.StatusBadRequest, "invalid request parameters", err)
}

// wrapBodyError classifies recognizable read failures and preserves other I/O
// errors for the application. It never classifies by matching error text.
func wrapBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return wrapRequestError(http.StatusRequestEntityTooLarge, "request body too large", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return wrapRequestError(http.StatusBadRequest, "invalid request body", err)
	}
	return err
}
