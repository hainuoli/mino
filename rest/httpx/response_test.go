package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hainuoli/mino/core/jsonx"
	"github.com/hainuoli/mino/rest"
	"github.com/hainuoli/mino/rest/middleware"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOk(t *testing.T) {
	tests := []struct {
		name  string
		value any
		body  string
	}{
		{"object", map[string]int{"id": 42}, `{"id":42}`},
		{"empty string", "", `""`},
		{"nil", nil, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			w := httptest.NewRecorder()
			Ok(w, tt.value)
			ast.Equal(http.StatusOK, w.Code)
			ast.Equal(tt.body, w.Body.String())
			ast.Equal("application/json", w.Header().Get("Content-Type"))
		})
	}
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
		Ok(w, marshalCheck(func() ([]byte, error) {
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
		writeErr  error
		wantLogs  int
		wantTrace any
	}{
		{
			name:     "encode failure",
			write:    func(ctx context.Context, w http.ResponseWriter) { OkCtx(ctx, w, make(chan int)) },
			wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:     "invalid status",
			write:    func(ctx context.Context, w http.ResponseWriter) { JSONCtx(ctx, w, 99, nil) },
			wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:     "write failure",
			write:    func(ctx context.Context, w http.ResponseWriter) { JSONCtx(ctx, w, 201, struct{}{}) },
			writeErr: io.ErrClosedPipe, wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:     "fallback write failure",
			write:    func(ctx context.Context, w http.ResponseWriter) { OkCtx(ctx, w, make(chan int)) },
			writeErr: io.ErrClosedPipe, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name: "client error response encoding and fallback failure",
			write: func(ctx context.Context, w http.ResponseWriter) {
				SetErrorHandler(func(*http.Request, error) (int, any) { return 400, make(chan int) })
				r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
				Err(w, r, errors.New("bad request"))
			},
			writeErr: io.ErrClosedPipe, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name: "server error response write failure",
			write: func(ctx context.Context, w http.ResponseWriter) {
				r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
				Err(w, r, errors.New("backend failure"))
			},
			writeErr: io.ErrClosedPipe, wantLogs: 2, wantTrace: "trace-42",
		},
		{
			name:     "write after handler timeout",
			write:    func(ctx context.Context, w http.ResponseWriter) { OkCtx(ctx, w, struct{}{}) },
			writeErr: fmt.Errorf("write: %w", http.ErrHandlerTimeout),
		},
		{
			name:     "fallback after handler timeout",
			write:    func(ctx context.Context, w http.ResponseWriter) { OkCtx(ctx, w, make(chan int)) },
			writeErr: http.ErrHandlerTimeout, wantLogs: 1, wantTrace: "trace-42",
		},
		{
			name:     "simple entry uses background",
			write:    func(_ context.Context, w http.ResponseWriter) { JSON(w, http.StatusOK, make(chan int)) },
			wantLogs: 1,
		},
		{
			name:     "success shortcut uses background",
			write:    func(_ context.Context, w http.ResponseWriter) { Ok(w, make(chan int)) },
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
			if tt.writeErr != nil {
				w = &observedWriter{header: make(http.Header), err: tt.writeErr}
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
	Ok(w, req)
	ast.Equal(1, codec.unmarshalCalls)
	ast.Equal(1, codec.marshalCalls)
	codec.fail = true
	w = httptest.NewRecorder()
	Ok(w, req)
	ast.Equal(2, codec.marshalCalls) // Literal fallback bypasses the broken codec.
	ast.Equal(500, w.Code)
	ast.JSONEq(`{"message":"internal error"}`, w.Body.String())
}

type streamRecorder struct {
	*httptest.ResponseRecorder
	writeErr error
	flushErr error
	short    bool
	writes   int
	flushes  int
	onFlush  func()
}

func (w *streamRecorder) Write(p []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return w.ResponseRecorder.Write(p[:len(p)/2])
	}
	return w.ResponseRecorder.Write(p)
}

func (w *streamRecorder) FlushError() error {
	w.flushes++
	if w.onFlush != nil {
		w.onFlush()
	}
	if w.flushErr != nil {
		return w.flushErr
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestStream(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{"last step writes data", []string{"last\n"}},
		{"multiple steps", []string{"first\n", "second\n", "last\n"}},
		{"empty last step", []string{"first\n", ""}},
		{"empty stream", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			w := &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Stream", "mino")
			w.WriteHeader(http.StatusAccepted)
			var want bytes.Buffer
			calls := 0
			Stream(context.Background(), w, func(out io.Writer) bool {
				chunk := tt.chunks[calls]
				_, err := io.WriteString(out, chunk)
				require.NoError(t, err)
				want.WriteString(chunk)
				calls++
				return calls < len(tt.chunks)
			})
			ast.Equal(len(tt.chunks), calls)
			ast.Equal(calls, w.flushes)
			ast.Equal(want.String(), w.Body.String())
			ast.Equal(http.StatusAccepted, w.Code)
			ast.Equal("text/event-stream", w.Header().Get("Content-Type"))
			ast.Equal("mino", w.Header().Get("X-Stream"))
		})
	}
}

func TestStreamCanceledBeforeStart(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{canceled, expired} {
		w := &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
		Stream(ctx, w, func(io.Writer) bool {
			t.Fatal("a completed context must not invoke the callback")
			return false
		})
		assert.Zero(t, w.writes)
		assert.Zero(t, w.flushes)
		assert.Empty(t, w.Header())
	}
}

func TestStreamFailures(t *testing.T) {
	tests := []struct {
		name          string
		writeErr      error
		flushErr      error
		short         bool
		unsupported   bool
		cancelInStep  bool
		cancelOnFlush bool
		wantWrites    int
		wantFlushes   int
		wantError     error
	}{
		{name: "write", writeErr: io.ErrClosedPipe, wantWrites: 1, wantError: io.ErrClosedPipe},
		{name: "short write", short: true, wantWrites: 1, wantError: io.ErrShortWrite},
		{name: "flush", flushErr: io.ErrClosedPipe, wantWrites: 2, wantFlushes: 1, wantError: io.ErrClosedPipe},
		{name: "unsupported flush", unsupported: true, wantWrites: 2, wantError: http.ErrNotSupported},
		{name: "write after timeout", writeErr: fmt.Errorf("write: %w", http.ErrHandlerTimeout), wantWrites: 1},
		{name: "flush after timeout", flushErr: fmt.Errorf("flush: %w", http.ErrHandlerTimeout), wantWrites: 2, wantFlushes: 1},
		{name: "cancel in step", cancelInStep: true, wantWrites: 2},
		{name: "cancel after write failure", cancelInStep: true, writeErr: io.ErrClosedPipe, wantWrites: 1},
		{name: "cancel during flush", cancelOnFlush: true, flushErr: io.ErrClosedPipe, wantWrites: 2, wantFlushes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var logs bytes.Buffer
			h := &contextLogHandler{Handler: slog.NewJSONHandler(&logs, nil)}
			previous := slog.Default()
			slog.SetDefault(slog.New(h))
			t.Cleanup(func() { slog.SetDefault(previous) })
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), logContextKey{}, "stream-42"))
			defer cancel()
			w := &streamRecorder{
				ResponseRecorder: httptest.NewRecorder(),
				writeErr:         tt.writeErr,
				flushErr:         tt.flushErr,
				short:            tt.short,
			}
			if tt.cancelOnFlush {
				w.onFlush = cancel
			}
			var target http.ResponseWriter = w
			if tt.unsupported {
				// Hide both flushing methods without providing an Unwrap escape.
				target = struct{ http.ResponseWriter }{w}
			}
			calls := 0
			Stream(ctx, target, func(out io.Writer) bool {
				calls++
				if calls > 1 {
					t.Fatal("the stream continued after a terminal condition")
				}
				// Ignoring errors must not cause retries or lose the first failure.
				_, first := io.WriteString(out, "chunk\n")
				_, second := io.WriteString(out, "again\n")
				if first != nil {
					ast.ErrorIs(second, first)
				}
				if tt.cancelInStep {
					cancel()
				}
				return true
			})
			ast.Equal(1, calls)
			ast.Equal(tt.wantWrites, w.writes)
			ast.Equal(tt.wantFlushes, w.flushes)
			ast.NotContains(w.Body.String(), "internal error")
			if tt.wantError == nil {
				ast.Empty(logs.String())
			} else {
				ast.Equal([]any{"stream-42"}, h.traces)
				ast.Contains(logs.String(), tt.wantError.Error())
			}
		})
	}
}

func TestStreamPanic(t *testing.T) {
	w := &streamRecorder{ResponseRecorder: httptest.NewRecorder()}
	require.PanicsWithValue(t, "stream panic", func() {
		Stream(context.Background(), w, func(io.Writer) bool {
			panic("stream panic")
		})
	})
	assert.Zero(t, w.flushes)
}

// streamUnwrapper deliberately exposes no optional writer interfaces.
type streamUnwrapper struct{ http.ResponseWriter }

func (w streamUnwrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestStreamHTTP(t *testing.T) {
	tests := []struct {
		name       string
		http2      bool
		disconnect bool
	}{
		{"HTTP1 complete", false, false},
		{"HTTP1 disconnect", false, true},
		{"HTTP2 complete", true, false},
		{"HTTP2 disconnect", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			release := make(chan struct{})
			done := make(chan struct{})
			mux := rest.NewMux()
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			mux.Use(middleware.Logger(middleware.WithLogger(logger)), middleware.Recovery())
			mux.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusAccepted)
				calls := 0
				Stream(r.Context(), streamUnwrapper{w}, func(out io.Writer) bool {
					if calls == 1 {
						select {
						case <-release:
						case <-r.Context().Done():
							return false
						}
					}
					calls++
					_, err := fmt.Fprintf(out, "%d\n", calls)
					return err == nil && calls < 2
				})
			})
			server := httptest.NewUnstartedServer(mux)
			if tt.http2 {
				server.EnableHTTP2 = true
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream", nil)
			require.NoError(t, err)
			resp, err := server.Client().Do(r)
			require.NoError(t, err)
			defer resp.Body.Close()
			ast.Equal(http.StatusAccepted, resp.StatusCode)
			ast.Equal("application/x-ndjson", resp.Header.Get("Content-Type"))
			if tt.http2 {
				ast.Equal(2, resp.ProtoMajor)
			} else {
				ast.Equal(1, resp.ProtoMajor)
			}
			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			ast.Equal("1\n", line)
			select {
			case <-done:
				t.Fatal("the handler finished before the second chunk was released")
			default:
			}
			if tt.disconnect {
				require.NoError(t, resp.Body.Close())
			} else {
				close(release)
				tail, err := io.ReadAll(reader)
				require.NoError(t, err)
				ast.Equal("2\n", string(tail))
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("the stream did not stop after completion or client disconnect")
			}
		})
	}
}

func ExampleStream() {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	n := 0
	Stream(context.Background(), w, func(out io.Writer) bool {
		n++
		_, err := fmt.Fprintf(out, "data: %d\n\n", n)
		return err == nil && n < 2
	})
	fmt.Print(w.Body.String())
	// Output:
	// data: 1
	//
	// data: 2
}
