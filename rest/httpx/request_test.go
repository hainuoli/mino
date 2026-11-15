package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hainuoli/mino/core/mapping"
	"github.com/hainuoli/mino/rest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSources(t *testing.T) {
	ast := assert.New(t)
	var req struct {
		Value string `json:"value" form:"value" query:"value" header:"X-Value" path:"value"`
		Trace string `json:"-" header:"X-Trace"`
		Keep  string `query:"missing"`
	}
	req.Keep = "default"
	r := httptest.NewRequest(http.MethodPost, "/?value=query", strings.NewReader(`{"value":"body","Trace":"untrusted"}`))
	r.Header.Set("Content-Type", "application/problem+json; charset=utf-8")
	r.Header.Set("X-Value", "header")
	r.Header.Set("X-Trace", "trace")
	r.SetPathValue("value", "path")
	require.NoError(t, Parse(r, &req))
	ast.Equal("path", req.Value)
	ast.Equal("trace", req.Trace)
	ast.Equal("default", req.Keep)

	r = httptest.NewRequest(http.MethodPost, "/?value=query", strings.NewReader("value=form"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, ParseForm(r, &req))
	ast.Equal("form", req.Value)
	require.NoError(t, ParseQuery(r, &req))
	ast.Equal("query", req.Value)
}

func TestParseSourceIsolation(t *testing.T) {
	type request struct {
		ID    string `path:"id" json:"-"`
		Trace string `header:"X-Trace-Id" json:"-"`
		Page  int    `query:"page" json:"-"`
		Name  string `json:"name"`
	}
	tests := []struct {
		name  string
		setup func(*http.Request)
		want  request
	}{
		{
			name:  "missing sources preserve defaults",
			setup: func(*http.Request) {},
			want:  request{ID: "default", Page: 1, Name: "Mino"},
		},
		{
			name: "present sources bind independently",
			setup: func(r *http.Request) {
				r.SetPathValue("id", "42")
				r.Header.Set("X-Trace-Id", "trace-42")
				r.URL.RawQuery = "page=2"
			},
			want: request{ID: "42", Trace: "trace-42", Page: 2, Name: "Mino"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			r := httptest.NewRequest("POST", "/", strings.NewReader(`{"ID":"body","Trace":"body","Page":99,"name":"Mino"}`))
			r.Header.Set("Content-Type", "application/json")
			tt.setup(r)
			input := request{ID: "default", Page: 1}
			require.NoError(t, Parse(r, &input))
			ast.Equal(tt.want, input)
		})
	}
}

type observedBody struct {
	io.Reader
	reads, closes int
}

func (b *observedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}
func (b *observedBody) Close() error { b.closes++; return nil }

func TestParseTargetsAndBodyOwnership(t *testing.T) {
	ast := assert.New(t)
	for _, target := range []any{nil, struct{}{}, (*struct{})(nil)} {
		r := httptest.NewRequest("POST", "/", nil)
		body := &observedBody{Reader: strings.NewReader(`{}`)}
		r.Body = body
		r.Header.Set("Content-Type", "application/json")
		err := Parse(r, target)
		require.ErrorIs(t, err, mapping.ErrInvalidTarget)
		status, _ := DefaultErrorHandler(r, err)
		ast.Equal(500, status)
		ast.Zero(body.reads)
		ast.Zero(body.closes)
	}
	for _, contentType := range []string{"application/json", "text/plain", ""} {
		r := httptest.NewRequest("POST", "/", nil)
		body := &observedBody{Reader: strings.NewReader(`{}`)}
		r.Body, r.ContentLength = body, -1
		r.Header.Set("Content-Type", contentType)
		var req struct{}
		require.NoError(t, Parse(r, &req))
		ast.Zero(body.closes)
		if contentType == "application/json" {
			ast.Positive(body.reads)
		} else {
			ast.Zero(body.reads)
		}
	}
	var values []int
	require.NoError(t, ParseJSON(httptest.NewRequest("POST", "/", strings.NewReader(`[1,2]`)), &values))
	ast.Equal([]int{1, 2}, values)
}

func TestParseJSON(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
	}{
		{"empty", "", 0},
		{"valid", `{}`, 0},
		{"syntax", `{`, 400},
		{"type", `{"age":"secret"}`, 400},
		{"trailing", `{} {}`, 400},
		{"at limit", `{"padding":"` + strings.Repeat("x", maxJSONBody-14) + `"}`, 0},
		{"over limit", strings.Repeat("x", maxJSONBody+1), 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			var req struct{ Age int }
			err := ParseJSON(httptest.NewRequest("POST", "/", strings.NewReader(tt.body)), &req)
			if tt.status == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			status, body := DefaultErrorHandler(nil, err)
			ast.Equal(tt.status, status)
			ast.NotContains(fmt.Sprint(body), "secret")
			if tt.name == "type" {
				var typeErr *json.UnmarshalTypeError
				ast.ErrorAs(err, &typeErr)
			}
		})
	}
}

func TestParseMaxJSONBody(t *testing.T) {
	for name, parse := range map[string]func(*http.Request, any, ...ParseOption) error{
		"Parse": Parse, "ParseJSON": ParseJSON,
	} {
		t.Run(name, func(t *testing.T) {
			ast := assert.New(t)
			tests := []struct {
				name   string
				body   string
				opts   []ParseOption
				status int
			}{
				{"default", `{}`, nil, 0},
				{"exact limit", `{}`, []ParseOption{WithMaxJSONBody(2)}, 0},
				{"over limit", `{}`, []ParseOption{WithMaxJSONBody(1)}, 413},
				{"zero empty", "", []ParseOption{WithMaxJSONBody(0)}, 0},
				{"zero nonempty", `{}`, []ParseOption{WithMaxJSONBody(0)}, 413},
				{"negative", `{}`, []ParseOption{WithMaxJSONBody(-1)}, 500},
				{"last option wins", `{}`, []ParseOption{WithMaxJSONBody(-1), nil, WithMaxJSONBody(2)}, 0},
				{"last option invalid", `{}`, []ParseOption{WithMaxJSONBody(2), WithMaxJSONBody(-1)}, 500},
				{"maximum int64", `{}`, []ParseOption{WithMaxJSONBody(math.MaxInt64)}, 0},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					ast := assert.New(t)
					r := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
					r.Header.Set("Content-Type", "application/json")
					r.ContentLength = -1
					var dst struct{}
					err := parse(r, &dst, tt.opts...)
					if tt.status == 0 {
						require.NoError(t, err)
					} else {
						require.Error(t, err)
						status, _ := DefaultErrorHandler(r, err)
						ast.Equal(tt.status, status)
					}
				})
			}

			// Raising one call's limit must not change the next call's default.
			payload := strings.Repeat(" ", maxJSONBody) + `{}`
			for _, opts := range [][]ParseOption{nil, {WithMaxJSONBody(int64(len(payload)))}, nil} {
				r := httptest.NewRequest("POST", "/", strings.NewReader(payload))
				r.Header.Set("Content-Type", "application/json")
				var dst struct{}
				err := parse(r, &dst, opts...)
				if opts == nil {
					require.Error(t, err)
					status, _ := DefaultErrorHandler(r, err)
					ast.Equal(413, status)
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}

func TestParseLimitReadBounds(t *testing.T) {
	ast := assert.New(t)
	for _, parse := range []func(*http.Request, any, ...ParseOption) error{Parse, ParseJSON} {
		for _, limit := range []int64{-1, 4} {
			source := strings.NewReader(strings.Repeat("x", 100))
			body := &observedBody{Reader: source}
			r := httptest.NewRequest("POST", "/", nil)
			r.Header.Set("Content-Type", "application/json")
			r.Body, r.ContentLength = body, -1
			var dst struct{}
			require.Error(t, parse(r, &dst, WithMaxJSONBody(limit)))
			if limit < 0 {
				ast.Zero(body.reads)
			} else {
				ast.Equal(int(limit+1), 100-source.Len())
			}
			ast.Zero(body.closes)
		}
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

func TestParseReadFailures(t *testing.T) {
	ast := assert.New(t)
	tests := []struct {
		err    error
		status int
	}{
		{io.ErrUnexpectedEOF, 400},
		{context.Canceled, 499},
		{context.DeadlineExceeded, 500},
		{errors.New("transport failure"), 500},
		{&http.MaxBytesError{Limit: 1}, 413},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("POST", "/", failedReader{tt.err})
		var req struct{}
		err := ParseJSON(r, &req)
		ast.ErrorIs(err, tt.err)
		status, _ := DefaultErrorHandler(r, err)
		ast.Equal(tt.status, status)
	}
	for _, contentType := range []string{"application/json", "application/x-www-form-urlencoded"} {
		r := httptest.NewRequest("POST", "/", strings.NewReader("value=long"))
		r.Header.Set("Content-Type", contentType)
		r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 2)
		var req struct{}
		status, _ := DefaultErrorHandler(r, Parse(r, &req))
		ast.Equal(413, status)
	}
}

func TestParseBadInput(t *testing.T) {
	ast := assert.New(t)
	tests := []struct {
		query, contentType string
	}{
		{"?n=128", ""},
		{"?n=%zz", ""},
		{"", "application/json; broken"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/"+tt.query, nil)
		r.Header.Set("Content-Type", tt.contentType)
		var req struct {
			N int8 `query:"n"`
		}
		err := Parse(r, &req)
		require.Error(t, err)
		status, _ := DefaultErrorHandler(r, err)
		ast.Equal(400, status)
	}
	var unsupported struct {
		N map[string]int `query:"n"`
	}
	err := ParseQuery(httptest.NewRequest("GET", "/?n=1", nil), &unsupported)
	status, _ := DefaultErrorHandler(nil, err)
	ast.Equal(500, status)
}

type validatedRequest struct {
	ID     string `path:"id"`
	called bool
	err    error
}

func (v *validatedRequest) Validate() error {
	v.called = true
	if v.ID == "" {
		return errors.New("missing ID")
	}
	return v.err
}

func TestParseValidation(t *testing.T) {
	ast := assert.New(t)
	tests := []struct {
		err    error
		status int
	}{
		{nil, 0}, {errors.New("business detail"), 400},
		{Errorf(422, "invalid ID"), 422}, {context.Canceled, 499},
		{context.DeadlineExceeded, 500},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", "42")
		req := validatedRequest{err: tt.err}
		err := Parse(r, &req)
		ast.True(req.called)
		status, _ := DefaultErrorHandler(r, err)
		ast.Equal(tt.status, status)
	}
	req := validatedRequest{}
	require.NoError(t, ParsePath(httptest.NewRequest("GET", "/", nil), &req))
	ast.False(req.called)
}

func TestMuxIntegration(t *testing.T) {
	ast := assert.New(t)
	handler := func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID   string `path:"id" json:"-"`
			Name string `json:"name"`
		}
		if err := Parse(r, &req); err != nil {
			Err(w, r, err)
			return
		}
		OkJSONCtx(r.Context(), w, map[string]string{"id": req.ID, "name": req.Name})
	}
	mino := rest.NewMux()
	mino.Post("/users/{id}", handler)
	standard := http.NewServeMux()
	standard.HandleFunc("POST /users/{id}", handler)
	for _, mux := range []http.Handler{mino, standard} {
		r := httptest.NewRequest("POST", "/users/42", strings.NewReader(`{"name":"Mino","ID":"untrusted"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		ast.Equal(200, w.Code)
		ast.JSONEq(`{"id":"42","name":"Mino"}`, w.Body.String())
	}
}

func TestParseJSONTargets(t *testing.T) {
	number := 2
	tests := []struct {
		name   string
		body   string
		target func() any
		want   any
	}{
		{"struct", `{"Name":"Mino"}`, func() any { return &struct{ Name string }{} }, &struct{ Name string }{Name: "Mino"}},
		{"slice", `["Mino"]`, func() any { return new([]string) }, &[]string{"Mino"}},
		{"array", `["Mino"]`, func() any { return new([1]string) }, &[1]string{"Mino"}},
		{"map", `{"count":2}`, func() any { return new(map[string]int) }, &map[string]int{"count": 2}},
		{"scalar", `2`, func() any { return new(int) }, &number},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			r := httptest.NewRequest("POST", "/?unused=value", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			target := tt.target()
			require.NoError(t, Parse(r, target))
			ast.Equal(tt.want, target)
		})
	}
}

type batchRequest []string

var errEmptyBatch = errors.New("batch must contain an item")

func (batch *batchRequest) Validate() error {
	if len(*batch) == 0 {
		return errEmptyBatch
	}
	return nil
}

func TestParseBatchValidation(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		parse func(*http.Request, any, ...ParseOption) error
		want  error
	}{
		{"Parse validates after decoding", `["Mino"]`, Parse, nil},
		{"Parse rejects an invalid batch", `[]`, Parse, errEmptyBatch},
		{"ParseJSON only decodes", `[]`, ParseJSON, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			r := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			var batch batchRequest
			err := tt.parse(r, &batch)
			if tt.want == nil {
				require.NoError(t, err)
			} else {
				ast.ErrorIs(err, tt.want)
				status, _ := DefaultErrorHandler(r, err)
				ast.Equal(400, status)
			}
		})
	}
}

func TestParseSourceErrors(t *testing.T) {
	parse := func(r *http.Request, v any) error { return Parse(r, v) }
	tests := []struct {
		name   string
		parse  func(*http.Request, any) error
		setup  func(*http.Request)
		target any
		status int
	}{
		{
			name: "header conversion", parse: parse, status: 400,
			setup: func(r *http.Request) { r.Header.Set("X-Count", "invalid") },
			target: &struct {
				Count int `header:"X-Count"`
			}{},
		},
		{
			name: "path conversion", parse: parse, status: 400,
			setup: func(r *http.Request) { r.SetPathValue("count", "invalid") },
			target: &struct {
				Count int `path:"count"`
			}{},
		},
		{
			name: "query encoding", parse: ParseQuery, status: 400,
			setup: func(r *http.Request) { r.URL.RawQuery = "count=%zz" },
			target: &struct {
				Count int `query:"count"`
			}{},
		},
		{
			name: "form target must be a struct", parse: ParseForm, status: 500,
			setup: func(*http.Request) {}, target: new(int),
		},
		{
			name: "form target must be nonnil", parse: ParseForm, status: 500,
			setup: func(*http.Request) {}, target: (*struct{})(nil),
		},
		{
			name: "form input error precedes target error", parse: ParseForm, status: 400,
			setup: func(r *http.Request) { r.URL.RawQuery = "count=%zz" }, target: new(int),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			r := httptest.NewRequest("GET", "/", nil)
			tt.setup(r)
			err := tt.parse(r, tt.target)
			require.Error(t, err)
			status, _ := DefaultErrorHandler(r, err)
			ast.Equal(tt.status, status)
		})
	}
}

func TestParseAbsentBody(t *testing.T) {
	tests := []struct {
		name  string
		parse func(*http.Request, any, ...ParseOption) error
	}{
		{"Parse", Parse}, {"ParseJSON", ParseJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.Body = nil
			r.Header.Set("Content-Type", "application/json")
			var target struct{}
			require.NoError(t, tt.parse(r, &target))
		})
	}
}

func TestParseJSONInvalidTarget(t *testing.T) {
	tests := []any{nil, struct{}{}, (*struct{})(nil)}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%T", tt), func(t *testing.T) {
			ast := assert.New(t)
			body := &observedBody{Reader: strings.NewReader(`{}`)}
			r := httptest.NewRequest("POST", "/", nil)
			r.Body = body
			err := ParseJSON(r, tt)
			require.ErrorIs(t, err, mapping.ErrInvalidTarget)
			status, _ := DefaultErrorHandler(r, err)
			ast.Equal(500, status)
			ast.Zero(body.reads)
		})
	}
}

func ExampleParse() {
	var input struct {
		ID    string `path:"id" json:"-"`
		Trace string `header:"X-Trace-Id" json:"-"`
		Dry   bool   `query:"dry_run" json:"-"`
		Name  string `json:"name"`
	}
	r := httptest.NewRequest("POST", "/users/42?dry_run=true", strings.NewReader(`{"name":"Mino","ID":"body","Trace":"body","Dry":false}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Trace-Id", "trace-42")
	r.SetPathValue("id", "42")
	if err := Parse(r, &input); err != nil {
		panic(err)
	}
	fmt.Println(input.ID, input.Trace, input.Dry, input.Name)
	// Output: 42 trace-42 true Mino
}

func ExampleParseJSON() {
	// Explicit JSON decoding does not require a Content-Type header and does
	// not invoke validation. Parse selects sources and validates the target.
	var names []string
	r := httptest.NewRequest("POST", "/", strings.NewReader(`["Mino"]`))
	if err := ParseJSON(r, &names); err != nil {
		panic(err)
	}
	fmt.Println(names)
	// Output: [Mino]
}
