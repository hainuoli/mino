package rest

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hainuoli/mino/rest/chain"

	"github.com/stretchr/testify/assert"
)

// noop is the empty handler used when registering routes.
var noop = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

// errInvalidRoot drives the registration diagnostics asserted by the
// tree tests; production validates patterns earlier.
var errInvalidRoot = errors.New("pattern should start with /")

// addRoute is a test shorthand registering pattern through the
// production addRoutePath entry point.
func (n *node) addRoute(pattern string, handler http.Handler) *endpoint {
	start := strings.IndexByte(pattern, '/')
	if start < 0 {
		panic(fmt.Errorf("mino: %w: %q", errInvalidRoot, pattern))
	}
	return n.addRoutePath(pattern, pattern[start:], handler, false)
}

// addSubRoute is a test shorthand mounting handler at the normalized
// prefix used by Route and Mount.
func (n *node) addSubRoute(prefix string, handler http.Handler) {
	if prefix == "" || prefix[0] != '/' {
		panic(fmt.Errorf("mino: invalid mount prefix %q: %w", prefix, errInvalidRoot))
	}
	n.addSubRoutePath(prefix, prefix, handler)
}

// mockSub makes a mounted subtree satisfy http.Handler while
// routes() can assert it back as Routes, so SubRoutes round-trips.
type mockSub struct {
	routes []Route
}

func (s mockSub) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (s mockSub) Routes() []Route                              { return s.routes }
func (s mockSub) Middlewares() []chain.Middleware              { return nil }

func TestAddRoutePanics(t *testing.T) {
	n := &node{}

	assert.PanicsWithError(t, `mino: pattern should start with /: "ping"`, func() {
		n.addRoute("ping", noop)
	})

	n.addRoute("/api", noop)
	assert.PanicsWithError(t, `mino: duplicate route "/api": conflicts with registered pattern "/api"`, func() {
		n.addRoute("/api", noop)
	})
}

func TestInsertEmpty(t *testing.T) {
	n := &node{}
	assert.Same(t, n, n.insert(""))
}

func TestAddSubRoute(t *testing.T) {
	n := &node{}
	n.addSubRoute("/api", noop)

	ep, full := n.search("/api", nil)
	if assert.NotNil(t, ep) {
		assert.True(t, ep.subroute)
		assert.True(t, full)
	}

	ep, full = n.search("/api/users", nil)
	if assert.NotNil(t, ep) {
		assert.True(t, ep.internal)
		assert.False(t, full)
	}

	assert.PanicsWithError(t, `mino: duplicate route at mount prefix "/api": conflicts with registered pattern "/api"`, func() {
		n.addSubRoute("/api", noop)
	})
}

func TestAddSubRouteRoot(t *testing.T) {
	n := &node{}
	assert.PanicsWithError(t, `mino: invalid mount prefix "api": pattern should start with /`, func() {
		n.addSubRoute("api", noop)
	})
	assert.PanicsWithError(t, `mino: invalid mount prefix "": pattern should start with /`, func() {
		n.addSubRoute("", noop)
	})

	n.addSubRoute("/", noop)

	ep, full := n.search("/", nil)
	if assert.NotNil(t, ep) {
		assert.True(t, ep.subroute)
		assert.True(t, full)
	}

	ep, _ = n.search("/deep", nil)
	if assert.NotNil(t, ep) {
		assert.True(t, ep.internal)
	}
}

func TestAddSubRouteConflictsWithCatchAll(t *testing.T) {
	// A mount prefix colliding with an existing catch-all panics too.
	n := &node{}
	n.addRoute("/a/{rest...}", noop)
	assert.PanicsWithError(t,
		`mino: duplicate route at mount prefix "/a": conflicts with registered pattern "/a/{rest...}"`,
		func() { n.addSubRoute("/a", noop) })
}

func TestRoutes(t *testing.T) {
	ping := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ping"))
	})
	users := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("users"))
	})
	sub := &mockSub{routes: []Route{{Method: http.MethodGet, Pattern: "/pong"}}}

	n := &node{}
	n.addRoute("GET /ping", ping)
	n.addRoute("GET /api/users", users)
	n.addSubRoute("/api", sub)

	index := func(routes []Route) map[string]Route {
		byPattern := make(map[string]Route, len(routes))
		for _, r := range routes {
			byPattern[r.Pattern] = r
		}
		return byPattern
	}

	routes := n.routes("", http.MethodGet, "")
	assert.Len(t, routes, 3)

	byPattern := index(routes)

	if r, ok := byPattern["/ping"]; assert.True(t, ok) {
		assert.Equal(t, http.MethodGet, r.Method)
		w := httptest.NewRecorder()
		r.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
		assert.Equal(t, "ping", w.Body.String())
	}
	if r, ok := byPattern["/api/users"]; assert.True(t, ok) {
		w := httptest.NewRecorder()
		r.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/users", nil))
		assert.Equal(t, "users", w.Body.String())
	}
	if r, ok := byPattern["/api"]; assert.True(t, ok) {
		assert.Nil(t, r.Handler)
		assert.Equal(t, sub, r.SubRoutes)
	}

	byPattern = index(n.routes("", http.MethodGet, "/api"))
	for _, pattern := range []string{"/", "/ping", "/users"} {
		assert.Contains(t, byPattern, pattern)
	}
}

func TestSearchRoute(t *testing.T) {
	patterns := []string{
		"/ping",
		"/api/users",
		"/api/{id}",
		"/api/{id}/one",
		"/api/{id}/two",
		"/files/{name...}",
		"/{a}/{b}",
		"/slash/{$}",
	}

	tests := []struct {
		path   string
		want   string
		full   bool
		params []string
	}{
		{path: "/ping", want: "/ping", full: true},
		{path: "/api/users", want: "/api/users", full: true},
		{path: "/api/52", want: "/api/{id}", full: true, params: []string{"52"}},
		{path: "/api/52/one", want: "/api/{id}/one", full: true, params: []string{"52"}},
		{path: "/api/", want: ""},
		{path: "/files/a/b", want: "/files/{name...}", params: []string{"a/b"}},
		{path: "/files/", want: "/files/{name...}", full: true, params: []string{""}},
		{path: "/x/y", want: "/{a}/{b}", full: true, params: []string{"x", "y"}},
		{path: "/slash/", want: "/slash/{$}", full: true},
		{path: "/files"},
		{path: "/slash"},
		{path: "/missing"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			n := &node{}
			for _, pattern := range patterns {
				n.addRoute(pattern, noop)
			}

			values := make([]string, 16)
			ep, full := n.search(test.path, values)
			if test.want == "" {
				assert.Nil(t, ep)
				return
			}
			if !assert.NotNil(t, ep) {
				return
			}
			assert.Equal(t, test.want, ep.pattern)
			assert.Equal(t, test.full, full)
			for i, param := range test.params {
				assert.Equal(t, param, values[i])
			}
		})
	}
}

func TestSearchSubrouteCatchAll(t *testing.T) {
	n := &node{}
	n.addSubRoute("/api", noop)
	n.addRoute("/api/{id}/x", noop)

	values := make([]string, 16)

	// After the parameter branch is exhausted, fall back to the
	// mounted catch-all on the same node.
	ep, full := n.search("/api/52/rest", values)
	if assert.NotNil(t, ep) {
		assert.True(t, ep.internal)
		assert.False(t, full)
	}

	// A hit on the parameter branch's child wins over the catch-all.
	ep, full = n.search("/api/52/x", values)
	if assert.NotNil(t, ep) {
		assert.False(t, ep.internal)
		assert.True(t, full)
		assert.Equal(t, "52", values[0])
	}
}

func TestSearchRejectsBarePath(t *testing.T) {
	n := &node{}
	n.addRoute("/ping", noop)

	ep, full := n.search("", nil)
	assert.Nil(t, ep)
	assert.False(t, full)

	ep, full = n.search("ping", nil)
	assert.Nil(t, ep)
	assert.False(t, full)
}

func TestRouteHandler(t *testing.T) {
	var order []string

	base := http.NotFoundHandler()
	inner := &ChainHandler{handler: base}
	inner.compile([]chain.Middleware{tracer("inner", &order)})
	outer := &ChainHandler{handler: inner}
	outer.compile([]chain.Middleware{tracer("outer", &order)})

	h, mws := routeHandler(outer)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)

	final := chain.New(mws...).Then(echo("handler"))
	w = httptest.NewRecorder()
	final.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"outer", "inner"}, order)
	assert.Equal(t, "handler", w.Body.String())
}

func TestParsePattern(t *testing.T) {
	assert.Equal(t, []string{"id"}, parsePattern("/api/{id}"))
	assert.Equal(t, []string{"a", "b"}, parsePattern("/{a}/{b}"))
	assert.Empty(t, parsePattern("/api/users"))
	assert.PanicsWithError(t, `mino: invalid pattern "/{a}/{a}": parameter "a" is repeated`, func() {
		parsePattern("/{a}/{a}")
	})
}

func TestRoutePatternDiagnostics(t *testing.T) {
	for _, test := range []struct {
		pattern string
		err     string
	}{
		{"/files/{name}.txt", `mino: invalid pattern "GET /files/{name}.txt": segment "{name}.txt" must be a whole-segment parameter such as {name} or {name...}`},
		{"/files/{path...}/extra", `mino: invalid pattern "GET /files/{path...}/extra": segment "{path...}" is a catch-all and must be the final segment`},
		{"/{$}/extra", `mino: invalid pattern "GET /{$}/extra": segment "{$}" must be the final segment`},
		{"/{id}/{id}", `mino: invalid pattern "GET /{id}/{id}": parameter "id" is repeated`},
		{"/{1id}", `mino: invalid pattern "GET /{1id}": segment "{1id}" has an invalid parameter name; use a letter or underscore first, followed by letters, digits, or underscores`},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			assert.PanicsWithError(t, test.err, func() {
				NewMux().Get(test.pattern, noop)
			})
		})
	}

	// Segment validation runs before registration; duplicates fail afterwards.
	m := NewMux()
	m.Get("/users/{id}", noop)
	assert.PanicsWithError(t, `mino: duplicate route "GET /users/{name}": conflicts with registered pattern "GET /users/{id}"`, func() {
		m.Get("/users/{name}", noop)
	})
}

func TestParseSegment(t *testing.T) {
	tests := []struct {
		name    string
		segment string
		last    bool
		typ     nodeTyp
		param   string
		panics  bool
	}{
		{name: "static", segment: "users", typ: ntStatic},
		{name: "exact pin last", segment: "{$}", last: true, typ: ntStatic},
		{name: "exact pin not last", segment: "{$}", panics: true},
		{name: "param", segment: "{id}", typ: ntParam, param: "id"},
		{name: "catch-all last", segment: "{rest...}", last: true, typ: ntCatchAll, param: "rest"},
		{name: "catch-all not last", segment: "{rest...}", panics: true},
		{name: "empty braces", segment: "{}", panics: true},
		{name: "digit first", segment: "{1x}", panics: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pattern := "/" + test.segment
			if test.panics {
				assert.Panics(t, func() {
					parseSegment(pattern, test.segment, test.last)
				})
				return
			}
			typ, param := parseSegment(pattern, test.segment, test.last)
			assert.Equal(t, test.typ, typ)
			assert.Equal(t, test.param, param)
		})
	}
}

func TestValidWildcardName(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		{"empty", "", false},
		{"letter", "id", true},
		{"underscore", "_id", true},
		{"digit first", "1id", false},
		{"digit later", "id2", true},
		{"mixed", "user_id2", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.valid, validWildcardName(test.in))
		})
	}
}

func TestPatternToPath(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		basePath string
		want     string
	}{
		{"method only", "GET /ping", "", "/ping"},
		{"strip base", "GET /api/users", "/api", "/users"},
		{"base exact", "GET /api", "/api", "/"},
		{"outside base", "GET /other", "/api", "/other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, patternToPath(test.pattern, test.basePath))
		})
	}
}
