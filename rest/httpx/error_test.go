package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultErrorHandler(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"nil", nil, 0, ""},
		{"unknown", errors.New("database password"), 500, "internal error"},
		{"wrapped status", fmt.Errorf("private SQL: %w", Errorf(404, "user %s not found", "42")), 404, "user 42 not found"},
		{"server status", Errorf(503, "private backend"), 503, "internal error"},
		{"invalid status", Errorf(200, "success"), 500, "internal error"},
		{"canceled", fmt.Errorf("read: %w", context.Canceled), 499, ""},
		{"body limit", &http.MaxBytesError{Limit: 1}, 413, "request body too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			status, body := DefaultErrorHandler(nil, tt.err)
			ast.Equal(tt.status, status)
			if tt.message == "" {
				ast.Nil(body)
			} else {
				ast.Equal(map[string]string{"message": tt.message}, body)
			}
		})
	}
}

func TestErrorHandler(t *testing.T) {
	ast := assert.New(t)
	t.Cleanup(func() { SetErrorHandler(nil) })
	domainErr := errors.New("domain error")
	SetErrorHandler(func(r *http.Request, err error) (int, any) {
		if errors.Is(err, domainErr) {
			return 409, map[string]string{"error": "conflict"}
		}
		return DefaultErrorHandler(r, err)
	})
	tests := []struct {
		err    error
		status int
		body   string
	}{
		{domainErr, 409, `{"error":"conflict"}`},
		{Errorf(403, "forbidden"), 403, `{"message":"forbidden"}`},
		{context.Canceled, 499, ""},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		Err(w, httptest.NewRequest("GET", "/", nil), tt.err)
		ast.Equal(tt.status, w.Code)
		ast.Equal(tt.body, w.Body.String())
	}
	SetErrorHandler(func(*http.Request, error) (int, any) {
		return 401, nil
	})
	w := httptest.NewRecorder()
	Err(w, httptest.NewRequest("GET", "/", nil), domainErr)
	ast.Equal(401, w.Code)
	ast.Empty(w.Body.String())
	SetErrorHandler(nil)
	w = httptest.NewRecorder()
	Err(w, httptest.NewRequest("GET", "/", nil), Errorf(404, "missing"))
	ast.Equal(404, w.Code)
	ast.JSONEq(`{"message":"missing"}`, w.Body.String())

	SetErrorHandler(func(*http.Request, error) (int, any) { return 200, "private" })
	w = httptest.NewRecorder()
	Err(w, httptest.NewRequest("GET", "/", nil), domainErr)
	ast.Equal(500, w.Code)
	ast.JSONEq(`{"message":"internal error"}`, w.Body.String())
	untouched := &observedWriter{header: make(http.Header)}
	Err(untouched, httptest.NewRequest("GET", "/", nil), nil)
	ast.Empty(untouched.statuses)
	ast.Zero(untouched.writes)
}

func TestErrorHandlerConcurrent(t *testing.T) {
	t.Cleanup(func() { SetErrorHandler(nil) })
	tests := []struct {
		name    string
		handler func(*http.Request, error) (int, any)
		status  int
		body    string
	}{
		{"default", nil, 404, `{"message":"missing"}`},
		{"custom", func(*http.Request, error) (int, any) {
			return 409, map[string]string{"message": "conflict"}
		}, 409, `{"message":"conflict"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			SetErrorHandler(tt.handler)
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 20 {
						w := httptest.NewRecorder()
						Err(w, httptest.NewRequest("GET", "/", nil), Errorf(404, "missing"))
						ast.Equal(tt.status, w.Code)
						ast.JSONEq(tt.body, w.Body.String())
					}
				}()
			}
			wg.Wait()
		})
	}
}

func ExampleSetErrorHandler() {
	// The application can enable detailed development logs while keeping the
	// default public response. No new error hook or Mino logger is required.
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	SetErrorHandler(func(r *http.Request, err error) (int, any) {
		status, body := DefaultErrorHandler(r, err)
		if status == http.StatusBadRequest {
			logger.DebugContext(r.Context(), "invalid request", "error", err)
		}
		return status, body
	})
	defer SetErrorHandler(nil)

	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"age":"invalid"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var input struct {
		Age int `json:"age"`
	}
	if err := Parse(r, &input); err != nil {
		Err(w, r, err)
	}
	fmt.Println(w.Code, w.Body.String())
	fmt.Println(strings.Contains(logs.String(), "decode JSON body"))
	// Output:
	// 400 {"message":"invalid request body"}
	// true
}
