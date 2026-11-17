package middleware

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hainuoli/mino/rest"
	"github.com/hainuoli/mino/rest/chain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryWritesBareFallback(t *testing.T) {
	ast := assert.New(t)
	captureDefaultLogger(t) // silence the panic log on the default sink
	h := chain.New(Recovery()).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999") // a handler may declare a length before panicking
		panic("boom")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	ast.Equal(http.StatusInternalServerError, w.Code)
	ast.Empty(w.Body.String())
	ast.Empty(w.Header().Get("Content-Length")) // stale header removed
	ast.Empty(w.Header().Get("Content-Type"))   // no envelope, no sniffing
}

func TestRecoveryCustomFallback(t *testing.T) {
	ast := assert.New(t)
	captureDefaultLogger(t) // silence the panic log on the default sink
	shaped := chain.New(Recovery(WithFallback(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"recovered"}`))
	}))).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	w := httptest.NewRecorder()
	shaped.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	ast.Equal(http.StatusServiceUnavailable, w.Code)
	ast.JSONEq(`{"message":"recovered"}`, w.Body.String())

	// A nil fallback restores the bare default.
	bare := chain.New(Recovery(WithFallback(nil))).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w = httptest.NewRecorder()
	bare.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	ast.Equal(http.StatusInternalServerError, w.Code)
	ast.Empty(w.Body.String())
}

func TestRecoveryPassthrough(t *testing.T) {
	ast := assert.New(t)
	h := chain.New(Recovery()).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7}`))
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	ast.Equal(http.StatusCreated, w.Code)
	ast.Equal(`{"id":7}`, w.Body.String())
}

func TestRecoveryPreservesErrAbortHandler(t *testing.T) {
	ast := assert.New(t)
	h := chain.New(Recovery()).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	w := httptest.NewRecorder()
	ast.PanicsWithValue(http.ErrAbortHandler, func() {
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	})
	ast.Equal(http.StatusOK, w.Code) // the server owns the teardown
}

func TestRecoveryAbortsAfterCommit(t *testing.T) {
	captureDefaultLogger(t)
	tests := []struct {
		name  string
		flush bool
	}{
		{"write", false},
		{"flush", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			fallback := false
			h := Recovery(WithFallback(func(http.ResponseWriter, *http.Request) {
				fallback = true
			}))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.flush {
					w.(http.Flusher).Flush()
				} else {
					_, _ = w.Write([]byte("partial"))
				}
				panic("mid-response")
			}))
			w := httptest.NewRecorder()
			ast.PanicsWithValue(http.ErrAbortHandler, func() {
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			})
			ast.False(fallback)
			ast.Equal(http.StatusOK, w.Code)
		})
	}
}

func TestRecoveryAbortsNetworkResponse(t *testing.T) {
	captureDefaultLogger(t)
	tests := []struct {
		name  string
		h2    bool
		proto int
	}{
		{"http1", false, 1},
		{"http2", true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			server := httptest.NewUnstartedServer(Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				panic("interrupted")
			})))
			server.EnableHTTP2 = tt.h2
			server.StartTLS()
			defer server.Close()
			resp, err := server.Client().Get(server.URL)
			require.NoError(t, err)
			defer resp.Body.Close()
			ast.Equal(tt.proto, resp.ProtoMajor)
			data, err := io.ReadAll(resp.Body)
			ast.Equal("partial", string(data))
			ast.Error(err)
		})
	}
}

func TestRecoveryLogsAndCallsHook(t *testing.T) {
	ast := assert.New(t)
	capture := captureDefaultLogger(t)

	var hooked struct {
		value any
		stack []byte
	}
	h := chain.New(Recovery(WithOnPanic(func(r *http.Request, v any, stack []byte) {
		hooked.value, hooked.stack = v, stack
	}))).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("sealed failure"))
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	records := capture.take()
	require.Len(t, records, 1)
	ast.Equal(slog.LevelError, records[0].level)
	ast.Equal("middleware: recovered from panic", records[0].message)
	err, ok := records[0].attrs["panic"].(error)
	require.True(t, ok)
	ast.EqualError(err, "sealed failure")
	ast.Contains(records[0].attrs["stack"], "goroutine")
	ast.EqualError(hooked.value.(error), "sealed failure")
	ast.Contains(string(hooked.stack), "goroutine")
	ast.Equal(http.StatusInternalServerError, w.Code)
}

func TestRecoveryUsesCurrentDefaultLogger(t *testing.T) {
	ast := assert.New(t)
	original := captureDefaultLogger(t)
	h := Recovery()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	current := &captureHandler{}
	slog.SetDefault(slog.New(current))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	ast.Equal(http.StatusInternalServerError, w.Code)
	ast.Empty(original.take())
	records := current.take()
	require.Len(t, records, 1)
	ast.Equal("middleware: recovered from panic", records[0].message)
}

func TestRecoveryWithMuxAndLoggerOrder(t *testing.T) {
	ast := assert.New(t)
	capture := captureDefaultLogger(t)

	mux := rest.NewMux()
	mux.Use(Logger(), Recovery()) // logger outside, recovery inside
	mux.Get("/boom", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	mux.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))
	ast.Equal(http.StatusInternalServerError, w.Code)
	ast.Empty(w.Body.String()) // bare status, no envelope

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
	ast.Equal(http.StatusOK, w.Code)

	records := capture.take()
	require.Len(t, records, 3) // panic log + two access logs
	ast.Equal("middleware: recovered from panic", records[0].message)
	ast.EqualValues(http.StatusInternalServerError, records[1].attrs["status"])
	ast.Equal("GET /boom", records[1].attrs["pattern"])
	ast.EqualValues(http.StatusOK, records[2].attrs["status"])
	ast.Equal("GET /ok", records[2].attrs["pattern"])
}

func TestRecoveryClosesAlreadyHijackedConnection(t *testing.T) {
	ast := assert.New(t)
	captureDefaultLogger(t)
	base := &flushHijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := Recovery()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		panic("hijacked handler failed")
	}))
	ast.PanicsWithValue(http.ErrAbortHandler, func() { h.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/", nil)) })
	ast.True(base.closed)
}

// captureDefaultLogger points the global slog default at a capture handler
// and restores the previous default on cleanup. Recovery logs through the
// process default, so tests need this redirect; the capture handler itself
// lives in logger_test.go.
func captureDefaultLogger(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}
