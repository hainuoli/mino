package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hainuoli/mino/core/errorx"
	"github.com/hainuoli/mino/core/jsonx"
)

// Ok writes status 200 without a response body.
func Ok(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
}

// OkJSON writes v as JSON with status 200, using context.Background for logs.
// Use OkJSONCtx to associate failure logs with a request.
func OkJSON(w http.ResponseWriter, v any) {
	writeJSON(context.Background(), w, http.StatusOK, v)
}

// OkJSONCtx behaves like OkJSON and uses ctx for failure logs.
func OkJSONCtx(ctx context.Context, w http.ResponseWriter, v any) {
	writeJSON(ctx, w, http.StatusOK, v)
}

// JSON marshals v before committing the response. Encoding failures write
// a fixed JSON 500 response; write failures are logged without retrying.
// The status must be 200 through 599. Statuses 204, 205, and 304 write no body.
// It does not add a business envelope or invoke the request-aware error handler
// for output failures, and it uses context.Background for logs; use JSONCtx for
// request context.
//
//	httpx.JSON(w, http.StatusOK, user)
//	httpx.JSON(w, http.StatusCreated, user)
func JSON(w http.ResponseWriter, status int, v any) {
	JSONCtx(context.Background(), w, status, v)
}

// JSONCtx behaves like JSON and uses ctx for encoding, fallback,
// and write failure logs. It does not require a Mino logger or tracing setup.
func JSONCtx(ctx context.Context, w http.ResponseWriter, status int, v any) {
	if status < 200 || status > 599 {
		slog.ErrorContext(ctx, "httpx: invalid JSON response status", "status", status)
		writeInternalServerError(ctx, w)
		return
	}
	writeJSON(ctx, w, status, v)
}

// Err writes an error response, using r.Context for server-error logging
// and any encoding, fallback, or write failures. r must not be nil.
// It does nothing for nil errors. Call it before committing the response.
func Err(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	status, body := errorHandler(r, err)
	if status < 400 || status > 599 {
		err = errorx.Wrapf(err, "invalid error response status %d", status)
		status, body = http.StatusInternalServerError, map[string]string{"message": "internal error"}
	}
	ctx := r.Context()
	if status >= 500 {
		slog.ErrorContext(ctx, "httpx: request failed", "status", status, "error", err)
	}
	if body == nil {
		w.WriteHeader(status)
		return
	}
	writeJSON(ctx, w, status, body)
}

// writeJSON receives a final status established by a response entry point.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	if status == http.StatusNoContent || status == http.StatusResetContent || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	data, err := jsonx.Marshal(v)
	if err != nil {
		slog.ErrorContext(ctx, "httpx: encode JSON response", "error", err)
		writeInternalServerError(ctx, w)
		return
	}
	writeResponse(ctx, w, status, data)
}

// writeInternalServerError writes a literal JSON 500 response, so the fallback
// stays usable even when the configured codec fails.
func writeInternalServerError(ctx context.Context, w http.ResponseWriter) {
	w.Header().Del("Content-Length")
	writeResponse(ctx, w, http.StatusInternalServerError, []byte(`{"message":"internal error"}`))
}

// writeResponse commits status and writes data as a JSON response.
func writeResponse(ctx context.Context, w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		slog.ErrorContext(ctx, "httpx: write JSON response", "error", err)
	}
}
