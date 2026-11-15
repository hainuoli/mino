package rest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hainuoli/mino/rest/chain"

	"github.com/stretchr/testify/assert"
)

// mockRoutes satisfies Routes and Middlewares over fixed slices, so
// Walk tests can drive arbitrary route trees.
type mockRoutes struct {
	routes []Route
	mws    []chain.Middleware
}

func (r *mockRoutes) Routes() []Route                 { return r.routes }
func (r *mockRoutes) Middlewares() []chain.Middleware { return r.mws }

func TestWalk(t *testing.T) {
	ast := assert.New(t)
	root := &mockRoutes{
		routes: []Route{
			{Method: http.MethodGet, Pattern: "/ping"},
			{Method: http.MethodGet, Pattern: "/api", SubRoutes: &mockRoutes{
				routes: []Route{
					{Method: http.MethodGet, Pattern: "/ping"},
					{Method: http.MethodGet, Pattern: "/:id"},
				},
			}},
		},
	}

	var walked []Route
	ast.NoError(Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))

	ast.Len(walked, 3)
	ast.Equal("/ping", walked[0].Pattern)
	ast.Equal("/api/ping", walked[1].Pattern)
	ast.Equal("/api/:id", walked[2].Pattern)
}

func TestWalkMiddlewares(t *testing.T) {
	ast := assert.New(t)
	root := &mockRoutes{
		mws: []chain.Middleware{marker("root")},
		routes: []Route{
			{Pattern: "/api", SubRoutes: &mockRoutes{
				mws:    []chain.Middleware{marker("api")},
				routes: []Route{{Pattern: "/ping"}},
			}},
		},
	}
	var walked []Route
	ast.NoError(Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))
	w := httptest.NewRecorder()
	h := chain.New(walked[0].Middlewares...).Then(echo("handler"))
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	ast.Equal([]string{"root", "api"}, w.Header().Values("X-Chain"))
	ast.Equal("handler", w.Body.String())
}

func TestWalkMuxInlineMiddlewares(t *testing.T) {
	ast := assert.New(t)
	m := NewMux()
	api := m.With(marker("entry")).Route("/api")
	api.With(marker("endpoint")).Get("/ping", echo("handler"))
	api.Use(marker("api"))
	m.Use(marker("root"))
	ast.Len(m.Middlewares(), 1)
	ast.Len(api.Middlewares(), 1)
	routes := api.Routes()
	if !ast.Len(routes, 1) {
		return
	}
	ast.Equal("/ping", routes[0].Pattern)
	ast.Len(routes[0].Middlewares, 1)
	want := []string{"root", "entry", "api", "endpoint"}
	ast.NoError(Walk(m, func(route Route) error {
		ast.Equal("/api/ping", route.Pattern)
		ast.Len(route.Middlewares, 4)
		w := httptest.NewRecorder()
		chain.New(route.Middlewares...).Then(route.Handler).ServeHTTP(w, httptest.NewRequest(http.MethodGet, route.Pattern, nil))
		ast.Equal(want, w.Header().Values("X-Chain"))
		ast.Equal("handler", w.Body.String())
		return nil
	}))
	w := serve(m, http.MethodGet, "/api/ping")
	ast.Equal(want, w.Header().Values("X-Chain"), "walking and serving must describe the same chain")
	ast.Equal("handler", w.Body.String())
}

func TestWalkHost(t *testing.T) {
	ast := assert.New(t)
	root := &mockRoutes{
		routes: []Route{
			{Method: http.MethodGet, Host: "example.com", Pattern: "/api", SubRoutes: &mockRoutes{
				routes: []Route{{Method: http.MethodGet, Pattern: "/ping"}},
			}},
			{Method: http.MethodGet, Pattern: "/plain"},
		},
	}
	var walked []Route
	ast.NoError(Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))
	ast.Len(walked, 2)
	ast.Equal("example.com", walked[0].Host)
	ast.Equal("", walked[1].Host)
}

func TestWalkAbort(t *testing.T) {
	ast := assert.New(t)
	errStop := errors.New("mino: stop")
	root := &mockRoutes{routes: []Route{
		{Pattern: "/first"},
		{Pattern: "/second"},
	}}
	called := 0
	err := Walk(root, func(r Route) error {
		called++
		return errStop
	})
	ast.Equal(errStop, err)
	ast.Equal(1, called)
}

func TestWalkAbortNested(t *testing.T) {
	ast := assert.New(t)
	errStop := errors.New("mino: stop")
	root := &mockRoutes{routes: []Route{{
		Pattern:   "/api",
		SubRoutes: &mockRoutes{routes: []Route{{Pattern: "/first"}, {Pattern: "/second"}}},
	}}}
	called := 0
	err := Walk(root, func(r Route) error {
		called++
		return errStop
	})
	ast.Equal(errStop, err)
	ast.Equal(1, called)
}

func TestWalkNilRoutes(t *testing.T) {
	ast := assert.New(t)
	called := false
	ast.NoError(Walk(nil, func(Route) error {
		called = true
		return nil
	}))
	ast.False(called)
}

func TestWalkNilFunc(t *testing.T) {
	ast := assert.New(t)
	root := &mockRoutes{routes: []Route{{
		Pattern:   "/api",
		SubRoutes: &mockRoutes{routes: []Route{{Pattern: "/ping"}}},
	}}}
	ast.NoError(Walk(root, nil))
}

func TestJoinMiddleware(t *testing.T) {
	ast := assert.New(t)
	joined := joinMiddleware(
		[]chain.Middleware{marker("a"), marker("b")},
		[]chain.Middleware{marker("c")},
	)

	w := httptest.NewRecorder()
	chain.New(joined...).Then(echo("handler")).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	ast.Equal([]string{"a", "b", "c"}, w.Header().Values("X-Chain"))
	ast.Equal("handler", w.Body.String())
	ast.Empty(joinMiddleware(nil, nil))
}

func TestJoinPattern(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{"both empty", "", "", ""},
		{"empty a", "", "/b", "/b"},
		{"root a", "/", "/b", "/b"},
		{"empty b", "/a", "", "/a"},
		{"root b", "/a", "/", "/a"},
		{"normal join", "/a", "/b", "/a/b"},
		{"dedup slashes", "/a/", "/b", "/a/b"},
		{"relative b", "/a", "b", "/a/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := assert.New(t)
			ast.Equal(tt.want, joinPattern(tt.a, tt.b))
		})
	}
}
