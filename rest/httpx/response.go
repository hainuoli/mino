package httpx

import (
	"context"
	"errors"
	"io"
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
// use JSONCtx to attach a request context. Writes rejected with
// http.ErrHandlerTimeout are not logged again.
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

// Stream calls step synchronously until it returns false or ctx is canceled.
// Each completed step is flushed, including the last, unless the context was
// canceled or a write failed. Set headers and any status before calling Stream;
// it adds no protocol framing or error response. An empty step still flushes.
//
// Write and flush failures stop the stream and log once using ctx. Canceled
// contexts and http.ErrHandlerTimeout stop it silently. Flush uses ResponseController,
// including through Unwrap; unsupported flushing is logged, not ignored.
//
// ctx and step must not be nil. A blocking step must observe ctx itself.
// The callback must use its writer synchronously and must not retain it after
// Stream returns. Panics propagate to the caller, so Recovery remains in charge.
//
//	ticker := time.NewTicker(15 * time.Second)
//	defer ticker.Stop()
//	w.Header().Set("Content-Type", "text/event-stream")
//	httpx.Stream(r.Context(), w, func(out io.Writer) bool {
//		select {
//		case <-r.Context().Done():
//			return false
//		case <-ticker.C:
//			_, err := io.WriteString(out, ": ping\n\n")
//			return err == nil
//		}
//	})
func Stream(ctx context.Context, w http.ResponseWriter, step func(io.Writer) bool) {
	out := &streamWriter{Writer: w}
	rc := http.NewResponseController(w)
	for ctx.Err() == nil {
		more := step(out)
		if ctx.Err() != nil {
			return
		}
		err := out.err
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, http.ErrHandlerTimeout) {
				slog.ErrorContext(ctx, "httpx: stream response", "error", err)
			}
			return
		}
		if !more {
			return
		}
	}
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
	if _, err := w.Write(data); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		slog.ErrorContext(ctx, "httpx: write JSON response", "error", err)
	}
}

// streamWriter retains the first write failure, even if step ignores it.
type streamWriter struct {
	io.Writer
	err error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}
