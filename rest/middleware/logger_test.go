package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hainuoli/mino/rest"
	"github.com/hainuoli/mino/rest/chain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggerLogsRequest(t *testing.T) {
	ast := assert.New(t)
	capture := &captureHandler{}
	h := chain.New(Logger(WithLogger(slog.New(capture)))).Then(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("pong"))
		}),
	)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping?verbose=1", nil))

	records := capture.take()
	require.Len(t, records, 1)
	rec := records[0]
	ast.Equal(slog.LevelInfo, rec.level)
	ast.Equal("request", rec.message)
	ast.Equal("GET", rec.attrs["method"])
	ast.Equal("/ping", rec.attrs["path"]) // query excluded
	ast.EqualValues(http.StatusOK, rec.attrs["status"])
	ast.Positive(rec.attrs["bytes"])
	duration, ok := rec.attrs["duration"].(time.Duration)
	require.True(t, ok)
	ast.GreaterOrEqual(duration, time.Duration(0))
	_, hasPattern := rec.attrs["pattern"]
	ast.False(hasPattern) // no Mino mux, no pattern
}

func TestLoggerLevelByStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantLevel slog.Level
	}{
		{"ok", http.StatusOK, slog.LevelInfo},
		{"client error", http.StatusNotFound, slog.LevelInfo},
		{"server error", http.StatusInternalServerError, slog.LevelError},
		{"unavailable", http.StatusServiceUnavailable, slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			capture := &captureHandler{}
			h := chain.New(Logger(WithLogger(slog.New(capture)))).Then(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
				}),
			)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			records := capture.take()
			require.Len(t, records, 1)
			ast.Equal(tt.wantLevel, records[0].level)
		})
	}
}

func TestLoggerFiltersByFinalLevel(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		level   func(int) slog.Level
		wantLog bool
	}{
		{"ok", http.StatusOK, nil, false},
		{"client error", http.StatusNotFound, nil, false},
		{"server error", http.StatusInternalServerError, nil, true},
		{"custom error", http.StatusOK, func(int) slog.Level { return slog.LevelError }, true},
		{"custom info", http.StatusInternalServerError, func(int) slog.Level { return slog.LevelInfo }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
			h := Logger(WithLogger(logger), WithLevelFunc(tt.level))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("response"))
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			ast.Equal(tt.status, w.Code)
			ast.Equal("response", w.Body.String())
			if !tt.wantLog {
				ast.Empty(output.String())
				return
			}
			var record map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &record))
			ast.Equal("ERROR", record["level"])
			ast.EqualValues(tt.status, record["status"])
		})
	}
}

func TestLoggerCapturesDefaultAtConstruction(t *testing.T) {
	ast := assert.New(t)
	original := captureDefaultLogger(t)
	h := Logger(WithLogger(nil))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	current := &captureHandler{}
	slog.SetDefault(slog.New(current))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	records := original.take()
	require.Len(t, records, 1)
	ast.Equal("request", records[0].message)
	ast.Empty(current.take())
}

func TestLoggerLogsRecoveredFailureWhenInfoDisabled(t *testing.T) {
	ast := assert.New(t)
	panicLogs := captureDefaultLogger(t)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelError}))
	h := chain.New(Logger(WithLogger(logger)), Recovery()).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	ast.Equal(http.StatusInternalServerError, w.Code)
	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	ast.Equal("ERROR", record["level"])
	ast.Equal("request", record["msg"])
	ast.EqualValues(http.StatusInternalServerError, record["status"])
	require.Len(t, panicLogs.take(), 1)
}

func TestLoggerOptions(t *testing.T) {
	ast := assert.New(t)

	// A custom level func overrides the default mapping.
	capture := &captureHandler{}
	h := chain.New(Logger(
		WithLogger(slog.New(capture)),
		WithLevelFunc(func(int) slog.Level { return slog.LevelWarn }),
	)).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	records := capture.take()
	require.Len(t, records, 1)
	ast.Equal(slog.LevelWarn, records[0].level)

	// Nil options fall back to the defaults instead of panicking.
	capture = &captureHandler{}
	h = chain.New(Logger(nil, WithLogger(slog.New(capture)), nil)).Then(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	)
	ast.NotPanics(func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	records = capture.take()
	require.Len(t, records, 1)
	ast.Equal(slog.LevelInfo, records[0].level)
}

func TestLoggerRecordsRoutePattern(t *testing.T) {
	ast := assert.New(t)
	capture := &captureHandler{}
	mux := rest.NewMux()
	mux.Use(Logger(WithLogger(slog.New(capture))))
	mux.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users/42", nil))
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	records := capture.take()
	require.Len(t, records, 2)
	ast.Equal("GET /users/{id}", records[0].attrs["pattern"])
	_, hasPattern := records[1].attrs["pattern"]
	ast.False(hasPattern) // a 404 never matched a route
	ast.EqualValues(http.StatusNotFound, records[1].attrs["status"])
}

func TestLoggerMarksInterruptedResponses(t *testing.T) {
	ast := assert.New(t)
	capture := &captureHandler{}
	h := Logger(WithLogger(slog.New(capture)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	}))
	ast.PanicsWithValue(http.ErrAbortHandler, func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) })
	records := capture.take()
	require.Len(t, records, 1)
	ast.EqualValues(200, records[0].attrs["status"])
	ast.Equal(true, records[0].attrs["aborted"])
	ast.EqualValues(7, records[0].attrs["bytes"])
}

func TestLoggerOptionsCanBeReusedConcurrently(t *testing.T) {
	options := []LoggerOption{WithLogger(nil), WithLevelFunc(nil)}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = Logger(options...) }()
	}
	wg.Wait()
}

// Log capture fixtures, shared with recovery_test.go and timeout_test.go.

type logRecord struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

// captureHandler collects log records so tests can assert on them.
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, message: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, rec)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// take returns a copy of the captured records.
func (h *captureHandler) take() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logRecord(nil), h.records...)
}
