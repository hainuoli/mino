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
	assert.NoError(t, Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))

	assert.Len(t, walked, 3)
	assert.Equal(t, "/ping", walked[0].Pattern)
	assert.Equal(t, "/api/ping", walked[1].Pattern)
	assert.Equal(t, "/api/:id", walked[2].Pattern)
}

func TestWalkMiddlewares(t *testing.T) {
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
	assert.NoError(t, Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))
	w := httptest.NewRecorder()
	h := chain.New(walked[0].Middlewares...).Then(echo("handler"))
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, []string{"root", "api"}, w.Header().Values("X-Chain"))
	assert.Equal(t, "handler", w.Body.String())
}

func TestWalkMuxInlineMiddlewares(t *testing.T) {
	m := NewMux()
	api := m.With(marker("entry")).Route("/api")
	api.With(marker("endpoint")).Get("/ping", echo("handler"))
	api.Use(marker("api"))
	m.Use(marker("root"))
	assert.Len(t, m.Middlewares(), 1)
	assert.Len(t, api.Middlewares(), 1)
	routes := api.Routes()
	if !assert.Len(t, routes, 1) {
		return
	}
	assert.Equal(t, "/ping", routes[0].Pattern)
	assert.Len(t, routes[0].Middlewares, 1)
	want := []string{"root", "entry", "api", "endpoint"}
	assert.NoError(t, Walk(m, func(route Route) error {
		assert.Equal(t, "/api/ping", route.Pattern)
		assert.Len(t, route.Middlewares, 4)
		w := httptest.NewRecorder()
		chain.New(route.Middlewares...).Then(route.Handler).ServeHTTP(w, httptest.NewRequest(http.MethodGet, route.Pattern, nil))
		assert.Equal(t, want, w.Header().Values("X-Chain"))
		assert.Equal(t, "handler", w.Body.String())
		return nil
	}))
	w := serve(m, http.MethodGet, "/api/ping")
	assert.Equal(t, want, w.Header().Values("X-Chain"), "walking and serving must describe the same chain")
	assert.Equal(t, "handler", w.Body.String())
}

func TestWalkHost(t *testing.T) {
	root := &mockRoutes{
		routes: []Route{
			{Method: http.MethodGet, Host: "example.com", Pattern: "/api", SubRoutes: &mockRoutes{
				routes: []Route{{Method: http.MethodGet, Pattern: "/ping"}},
			}},
			{Method: http.MethodGet, Pattern: "/plain"},
		},
	}
	var walked []Route
	assert.NoError(t, Walk(root, func(r Route) error {
		walked = append(walked, r)
		return nil
	}))
	assert.Len(t, walked, 2)
	assert.Equal(t, "example.com", walked[0].Host)
	assert.Equal(t, "", walked[1].Host)
}

func TestWalkAbort(t *testing.T) {
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
	assert.Equal(t, errStop, err)
	assert.Equal(t, 1, called)
}

func TestWalkAbortNested(t *testing.T) {
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
	assert.Equal(t, errStop, err)
	assert.Equal(t, 1, called)
}

func TestWalkNilRoutes(t *testing.T) {
	called := false
	assert.NoError(t, Walk(nil, func(Route) error {
		called = true
		return nil
	}))
	assert.False(t, called)
}

func TestWalkNilFunc(t *testing.T) {
	root := &mockRoutes{routes: []Route{{
		Pattern:   "/api",
		SubRoutes: &mockRoutes{routes: []Route{{Pattern: "/ping"}}},
	}}}
	assert.NoError(t, Walk(root, nil))
}

func TestJoinMiddleware(t *testing.T) {
	joined := joinMiddleware(
		[]chain.Middleware{marker("a"), marker("b")},
		[]chain.Middleware{marker("c")},
	)

	w := httptest.NewRecorder()
	chain.New(joined...).Then(echo("handler")).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"a", "b", "c"}, w.Header().Values("X-Chain"))
	assert.Equal(t, "handler", w.Body.String())
	assert.Empty(t, joinMiddleware(nil, nil))
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
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, joinPattern(test.a, test.b))
		})
	}
}
