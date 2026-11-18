package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hainuoli/mino/core/errorx"
	"github.com/hainuoli/mino/core/jsonx"
)

// Ok writes v as JSON with status 200. Failure logs use context.Background;
// use OkCtx to attach a request context.
//
//	httpx.Ok(w, map[string]int{"id": 42})
//	// body -> {"id":42}
func Ok(w http.ResponseWriter, v any) {
	JSONCtx(context.Background(), w, http.StatusOK, v)
}

// OkCtx writes v as JSON with status 200, using ctx for failure logs.
//
//	httpx.OkCtx(r.Context(), w, map[string]int{"id": 42})
func OkCtx(ctx context.Context, w http.ResponseWriter, v any) {
	JSONCtx(ctx, w, http.StatusOK, v)
}

// JSON writes v as JSON with the given status, adding no envelope. Encoding
// failures reply with a fixed JSON 500 rather than the error handler, and
// write failures are logged. Statuses 204, 205, and 304 write no body;
// invalid statuses fall back to 500. Failure logs use context.Background;
// use JSONCtx to attach a request context.
//
//	httpx.JSON(w, http.StatusCreated, map[string]int{"id": 42})
//	// body -> {"id":42}, status 201
func JSON(w http.ResponseWriter, status int, v any) {
	JSONCtx(context.Background(), w, status, v)
}

// JSONCtx writes v as JSON with the given status, using ctx for encoding,
// fallback, and write failure logs. See JSON for the full contract.
//
//	httpx.JSONCtx(r.Context(), w, http.StatusCreated, map[string]int{"id": 42})
func JSONCtx(ctx context.Context, w http.ResponseWriter, status int, v any) {
	if status < 200 || status > 599 {
		slog.ErrorContext(ctx, "httpx: invalid JSON response status", "status", status)
		writeInternalServerError(ctx, w)
		return
	}
	switch status {
	case http.StatusNoContent, http.StatusResetContent, http.StatusNotModified:
		w.WriteHeader(status)
		return
	}
	data, err := jsonx.Marshal(v)
	if err != nil {
		slog.ErrorContext(ctx, "httpx: encode JSON response", "error", err)
		writeInternalServerError(ctx, w)
		return
	}
	writeJSON(ctx, w, status, data)
}

// Err maps err through the configured error handler and writes the response.
// A nil err writes nothing, r must not be nil, 5xx errors log with r.Context,
// and the call must happen before the response is committed.
//
//	httpx.Err(w, r, httpx.Errorf(http.StatusNotFound, "user %d not found", 7))
//	// body -> {"message":"user 7 not found"}
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
	JSONCtx(ctx, w, status, body)
}

// writeInternalServerError replies with a literal JSON 500, so the fallback
// stays usable even when the codec is broken.
func writeInternalServerError(ctx context.Context, w http.ResponseWriter) {
	w.Header().Del("Content-Length")
	writeJSON(ctx, w, http.StatusInternalServerError, []byte(`{"message":"internal error"}`))
}

// writeJSON sets the JSON content type, commits status, and writes
// pre-encoded data.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		slog.ErrorContext(ctx, "httpx: write JSON response", "error", err)
	}
}
