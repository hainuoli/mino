package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hainuoli/mino/core/jsonx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOk(t *testing.T) {
	ast := assert.New(t)
	w := httptest.NewRecorder()
	Ok(w)
	ast.Equal(http.StatusOK, w.Code)
	ast.Empty(w.Body.String())
	ast.Empty(w.Header().Get("Content-Type"))
}

func TestJSON(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		value      any
		wantStatus int
		body       string
	}{
		{"created", 201, map[string]int{"id": 42}, 201, `{"id":42}`},
		{"nil", 200, nil, 200, `null`},
		{"encoding failure", 200, make(chan int), 500, `{"message":"internal error"}`},
		{"invalid status", 99, nil, 500, `{"message":"internal error"}`},
		{"no content", 204, make(chan int), 204, ""},
		{"reset content", 205, make(chan int), 205, ""},
		{"not modified", 304, make(chan int), 304, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			w := httptest.NewRecorder()
			if tt.name == "encoding failure" {
				w.Header().Set("Content-Length", "999")
			}
			JSON(w, tt.status, tt.value)
			ast.Equal(tt.wantStatus, w.Code)
			ast.Equal(tt.body, w.Body.String())
			if tt.body != "" {
				ast.Equal("application/json", w.Header().Get("Content-Type"))
			}
			if tt.name == "encoding failure" {
				ast.Empty(w.Header().Get("Content-Length"))
			}
		})
	}
}

type marshalCheck func() ([]byte, error)

func (f marshalCheck) MarshalJSON() ([]byte, error) { return f() }

type observedWriter struct {
	header   http.Header
	statuses []int
	writes   int
	err      error
}

func (w *observedWriter) Header() http.Header    { return w.header }
func (w *observedWriter) WriteHeader(status int) { w.statuses = append(w.statuses, status) }
func (w *observedWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.err != nil {
		return 0, w.err
	}
	return len(data), nil
}

func TestJSONCommitOrder(t *testing.T) {
	ast := assert.New(t)
	for _, writeErr := range []error{nil, io.ErrClosedPipe} {
		w := &observedWriter{header: make(http.Header), err: writeErr}
		OkJSON(w, marshalCheck(func() ([]byte, error) {
			ast.Empty(w.statuses)
			ast.Empty(w.header.Get("Content-Type"))
			return []byte(`{}`), nil
		}))
		ast.Equal([]int{200}, w.statuses)
		ast.Equal(1, w.writes) // Write failures do not start a second response.
	}
}

type logContextKey struct{}
type contextLogHandler struct {
	slog.Handler
	traces []any
}

func (h *contextLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.traces = append(h.traces, ctx.Value(logContextKey{}))
	return h.Handler.Handle(ctx, r)
}

func TestErrLogsRequestContext(t *testing.T) {
	ast := assert.New(t)
	var logs bytes.Buffer
	h := &contextLogHandler{Handler: slog.NewTextHandler(&logs, nil)}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), logContextKey{}, "trace-42"))
	w := httptest.NewRecorder()
	Err(w, r, errors.New("private database detail"))
	ast.Equal([]any{"trace-42"}, h.traces)
	ast.Contains(logs.String(), "private database detail")
	ast.NotContains(w.Body.String(), "database")
}

func TestJSONFailureLogsContext(t *testing.T) {
	tests := []struct {
		name      string
		write     func(context.Context, http.ResponseWriter)
		failWrite bool
		wantLogs  int
		wantTrace any
	}{
		{
			name:     "encode failure",
			write:    func(ctx context.Context, w http.ResponseWriter) { OkJSONCtx(ctx, w, make(chan int)) },
			wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:     "invalid status",
			write:    func(ctx context.Context, w http.ResponseWriter) { JSONCtx(ctx, w, 99, nil) },
			wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:      "write failure",
			write:     func(ctx context.Context, w http.ResponseWriter) { JSONCtx(ctx, w, 201, struct{}{}) },
			failWrite: true, wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:      "fallback write failure",
			write:     func(ctx context.Context, w http.ResponseWriter) { OkJSONCtx(ctx, w, make(chan int)) },
			failWrite: true, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name: "client error response encoding and fallback failure",
			write: func(ctx context.Context, w http.ResponseWriter) {
				SetErrorHandler(func(*http.Request, error) (int, any) { return 400, make(chan int) })
				r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
				Err(w, r, errors.New("bad request"))
			},
			failWrite: true, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name: "server error response write failure",
			write: func(ctx context.Context, w http.ResponseWriter) {
				r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
				Err(w, r, errors.New("backend failure"))
			},
			failWrite: true, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name:     "simple entry uses background",
			write:    func(_ context.Context, w http.ResponseWriter) { JSON(w, http.StatusOK, make(chan int)) },
			wantLogs: 1,
		},
		{
			name:     "success shortcut uses background",
			write:    func(_ context.Context, w http.ResponseWriter) { OkJSON(w, make(chan int)) },
			wantLogs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var logs bytes.Buffer
			h := &contextLogHandler{Handler: slog.NewTextHandler(&logs, nil)}
			previous := slog.Default()
			slog.SetDefault(slog.New(h))
			t.Cleanup(func() {
				slog.SetDefault(previous)
				SetErrorHandler(nil)
			})
			ctx := context.WithValue(context.Background(), logContextKey{}, "trace-42")
			var w http.ResponseWriter = httptest.NewRecorder()
			if tt.failWrite {
				w = &observedWriter{header: make(http.Header), err: io.ErrClosedPipe}
			}
			tt.write(ctx, w)
			require.Len(t, h.traces, tt.wantLogs)
			for _, trace := range h.traces {
				ast.Equal(tt.wantTrace, trace)
			}
		})
	}
}

type testCodec struct {
	marshalCalls, unmarshalCalls int
	fail                         bool
}

func (c *testCodec) Marshal(v any) ([]byte, error) {
	c.marshalCalls++
	if c.fail {
		return nil, errors.New("broken codec")
	}
	return json.Marshal(v)
}
func (c *testCodec) Unmarshal(b []byte, v any) error {
	c.unmarshalCalls++
	return json.Unmarshal(b, v)
}
func (*testCodec) Valid(b []byte) bool { return json.Valid(b) }

func TestJSONCodecIntegration(t *testing.T) {
	ast := assert.New(t)
	codec := &testCodec{}
	require.NoError(t, jsonx.SetCodec(codec))
	t.Cleanup(func() { _ = jsonx.SetCodec(&testCodec{}) })
	var req struct{}
	require.NoError(t, ParseJSON(httptest.NewRequest("POST", "/", bytes.NewBufferString(`{}`)), &req))
	w := httptest.NewRecorder()
	OkJSON(w, req)
	ast.Equal(1, codec.unmarshalCalls)
	ast.Equal(1, codec.marshalCalls)
	codec.fail = true
	w = httptest.NewRecorder()
	OkJSON(w, req)
	ast.Equal(2, codec.marshalCalls) // Literal fallback bypasses the broken codec.
	ast.Equal(500, w.Code)
	ast.JSONEq(`{"message":"internal error"}`, w.Body.String())
}
