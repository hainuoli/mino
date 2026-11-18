package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hainuoli/mino/rest"
	"github.com/hainuoli/mino/rest/httpx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestSizeLimits(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		length int64
		limit  int64
		status int
	}{
		{"below limit", "abc", 3, 4, 204},
		{"at limit", "abcd", 4, 4, 204},
		{"over limit", "abcde", 5, 4, 413},
		{"unknown length at limit", "abcd", -1, 4, 204},
		{"unknown length over limit", "abcde", -1, 4, 413},
		{"empty body with zero limit", "", 0, 0, 204},
		{"nonempty body with zero limit", "a", -1, 0, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var readErr error
			h := RequestSize(tt.limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, readErr = io.ReadAll(r.Body)
				if readErr != nil {
					httpx.Err(w, r, readErr)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			r.ContentLength = tt.length
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			ast.Equal(tt.status, w.Code)
			if tt.status == http.StatusRequestEntityTooLarge {
				var limitErr *http.MaxBytesError
				require.ErrorAs(t, readErr, &limitErr)
				ast.Equal(tt.limit, limitErr.Limit)
				ast.JSONEq(`{"message":"request body too large"}`, w.Body.String())
			} else {
				require.NoError(t, readErr)
			}
		})
	}
}

func TestRequestSizeRejectsNegativeLimit(t *testing.T) {
	ast := assert.New(t)
	ast.PanicsWithValue("middleware: request size must be non-negative", func() {
		RequestSize(-1)
	})
}

func TestRequestSizeAllowsNilBody(t *testing.T) {
	ast := assert.New(t)
	h := RequestSize(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ast.Nil(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Body = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	ast.Equal(http.StatusNoContent, w.Code)
}

// requestSizeBody counts Read and Close calls, so tests can assert that
// RequestSize performs neither.
type requestSizeBody struct {
	io.Reader
	reads  int
	closes int
}

func (b *requestSizeBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *requestSizeBody) Close() error {
	b.closes++
	return nil
}

func TestRequestSizeDoesNotReadOrCloseBody(t *testing.T) {
	ast := assert.New(t)
	body := &requestSizeBody{Reader: strings.NewReader("oversized")}
	h := RequestSize(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = body
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	ast.Equal(http.StatusNoContent, w.Code)
	ast.Zero(body.reads)
	ast.Zero(body.closes)
	require.NoError(t, r.Body.Close())
	ast.Equal(1, body.closes)
}

func TestRequestSizeWithParseJSON(t *testing.T) {
	mux := rest.NewMux()
	upload := func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if err := httpx.ParseJSON(r, &in); err != nil {
			httpx.Err(w, r, err)
			return
		}
		httpx.Ok(w, in)
	}
	mux.With(RequestSize(4)).Post("/limited", upload)
	mux.Post("/normal", upload)
	tests := []struct {
		path   string
		status int
	}{
		{"/limited", http.StatusRequestEntityTooLarge},
		{"/normal", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			ast := assert.New(t)
			r := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(`{"name":"Mino"}`))
			r.ContentLength = -1
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			ast.Equal(tt.status, w.Code)
		})
	}
}
