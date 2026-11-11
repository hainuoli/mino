package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hainuoli/mino/rest/chain"

	"github.com/stretchr/testify/assert"
)

// echo returns a handler writing s to the response body.
func echo(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(s))
	}
}

// recorder runs m against r and returns the recorder.
func recorder(m *Mux, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

// serve runs m against a fresh request, returning the recorder.
func serve(m *Mux, method, path string) *httptest.ResponseRecorder {
	return recorder(m, httptest.NewRequest(method, path, nil))
}

// marker returns a middleware stamping its name into the X-Chain
// response header, so tests can observe the chain per request.
func marker(name string) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-Chain", name)
			next.ServeHTTP(w, r)
		})
	}
}

// tracer returns a middleware appending its name to order before
// delegating, so tests can assert the call order end to end.
func tracer(name string, order *[]string) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*order = append(*order, name)
			next.ServeHTTP(w, r)
		})
	}
}

func compositionConf(mask int) MuxConf {
	return MuxConf{CleanPathRedirect: mask&1 != 0, EscapedPath: mask&2 != 0, TrailingSlashRedirect: mask&4 != 0}
}

func installCompositionFallbacks(router Router, name string, calls *[]string) {
	view := router.With(tracer("fallback", calls))
	view.NotFound(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fallback", name)
		http.Error(w, name+" not found", http.StatusNotFound)
	})
	view.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fallback", name)
		http.Error(w, name+" method not allowed", http.StatusMethodNotAllowed)
	})
}

// discardWriter is a ResponseWriter that drops the body and
// keeps only headers, for throughput benchmarks.
type discardWriter struct{ header http.Header }

func (w *discardWriter) Header() http.Header               { return w.header }
func (w *discardWriter) WriteHeader(int)                   {}
func (w *discardWriter) Write(p []byte) (int, error)       { return len(p), nil }
func (w *discardWriter) WriteString(s string) (int, error) { return len(s), nil }

// observedResponse captures what one request produced: status,
// headers, body and the routing facts behind it.
type observedResponse struct {
	status  int
	header  http.Header
	body    string
	pattern string
	values  []string
}

func observeRouting(h http.Handler, method, target, host string) observedResponse {
	// NewRequest parses CONNECT targets as authorities; set the method afterwards
	// to exercise path-form CONNECT with independent URL.Host and Request.Host.
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Method = method
	if host != "" {
		r.Host = host
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// ServeMux's automatic redirect output drifts across 1.23–1.27: 307
	// since 1.26, matched-pattern responses since 1.25, request-escaped
	// Location since 1.27. Reduce a redirect to its canonical status and
	// twice-decoded target so every stdlib generation compares; the exact
	// spelling is pinned by mino's own redirect tests.
	if w.Code == http.StatusMovedPermanently || w.Code == http.StatusTemporaryRedirect {
		path, query, hasQuery := strings.Cut(w.Header().Get("Location"), "?")
		for {
			decoded := unescapePath(path)
			if decoded == path {
				break
			}
			path = decoded
		}
		target := path
		if hasQuery {
			target += "?" + query
		}
		header := w.Header().Clone()
		header.Del("Location")
		return observedResponse{http.StatusMovedPermanently, header, target, "", nil}
	}
	return observedResponse{w.Code, w.Header(), w.Body.String(), r.Pattern,
		[]string{r.PathValue("id"), r.PathValue("rest"), r.PathValue("name")}}
}

func registerRoutingComparison(m *Mux, standard *http.ServeMux, patterns []string) {
	for _, pattern := range patterns {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s|%s|%s|%s", r.Pattern, r.PathValue("id"), r.PathValue("rest"), r.PathValue("name"))
		})
		m.Handle(pattern, h)
		standard.Handle(pattern, h)
	}
}

// requestSpec describes one request: method, target and host.
type requestSpec struct {
	method, target, host string
}

func (req requestSpec) request() *http.Request {
	r := httptest.NewRequest(http.MethodGet, req.target, nil)
	r.Method = req.method
	if req.host != "" {
		r.Host = req.host
	}
	return r
}

// handlerContextKey simulates an application-owned context value, so tests
// can assert routing preserves it across Handler and ServeHTTP.
type handlerContextKey struct{}

func standardRouteTable(patterns []string) (accepted bool) {
	defer func() {
		if recover() != nil {
			accepted = false
		}
	}()
	m := http.NewServeMux()
	for _, pattern := range patterns {
		m.Handle(pattern, http.NotFoundHandler())
	}
	return true
}

// routeTableOrders exhausts registration permutations for these small tables.
func routeTableOrders(patterns []string) [][]string {
	var orders [][]string
	order := slices.Clone(patterns)
	var permute func(int)
	permute = func(from int) {
		if from == len(order) {
			orders = append(orders, slices.Clone(order))
			return
		}
		for i := from; i < len(order); i++ {
			order[from], order[i] = order[i], order[from]
			permute(from + 1)
			order[from], order[i] = order[i], order[from]
		}
	}
	permute(0)
	return orders
}

func checkLegalRouteTable(t *testing.T, patterns []string, requests []requestSpec) {
	t.Helper()
	for _, order := range routeTableOrders(patterns) {
		m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching(), WithTrailingSlashRedirect())
		standard := http.NewServeMux()
		if !assert.NotPanics(t, func() {
			for _, pattern := range order {
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Route", pattern)
					fmt.Fprintf(w, "%s|%s|%s|%s|%s", r.Pattern, r.PathValue("id"), r.PathValue("name"), r.PathValue("rest"), r.PathValue("名字"))
				})
				standard.Handle(pattern, h)
				m.Handle(pattern, h)
			}
		}, "standard-library legal table rejected: %q", order) {
			return
		}
		// Matching aliases remain internal: public route inspection must still
		// report exactly the user-registered patterns, with their original spelling.
		var visible []string
		for _, route := range m.Routes() {
			pattern := route.Host + route.Pattern
			if route.Method != "" {
				pattern = route.Method + " " + pattern
			}
			visible = append(visible, pattern)
		}
		wantPatterns := slices.Clone(order)
		slices.Sort(wantPatterns)
		slices.Sort(visible)
		if !assert.Equal(t, wantPatterns, visible, "public routes: %q", order) {
			return
		}
		for _, request := range requests {
			want := observeRouting(standard, request.method, request.target, request.host)
			got := observeRouting(m, request.method, request.target, request.host)
			if !assert.Equal(t, want, got, "ServeHTTP: table=%q request=%+v", order, request) {
				return
			}
			r := request.request()
			before := r.Clone(r.Context())
			gotHandler, gotPattern := m.Handler(r)
			wantHandler, wantPattern := standard.Handler(before)
			if !assert.Equal(t, before, r, "Handler changed request: table=%q request=%+v", order, request) {
				return
			}
			w, wantW := httptest.NewRecorder(), httptest.NewRecorder()
			gotHandler.ServeHTTP(w, r.Clone(r.Context()))
			wantHandler.ServeHTTP(wantW, before.Clone(before.Context()))
			redirected := w.Code == http.StatusMovedPermanently || w.Code == http.StatusTemporaryRedirect ||
				wantW.Code == http.StatusMovedPermanently || wantW.Code == http.StatusTemporaryRedirect
			if redirected {
				// ServeMux's automatic redirect output drifts across
				// 1.23–1.27 (307 since 1.26, matched-pattern responses
				// since 1.25, request-escaped Location since 1.27):
				// require the same redirect to the same twice-decoded
				// target; spelling is pinned by mino's own redirect tests.
				target := func(location string) string {
					path, query, hasQuery := strings.Cut(location, "?")
					for {
						decoded := unescapePath(path)
						if decoded == path {
							break
						}
						path = decoded
					}
					if hasQuery {
						return path + "?" + query
					}
					return path
				}
				if !assert.Equal(t, http.StatusTemporaryRedirect, w.Code, "Handler redirect status: table=%q request=%+v", order, request) ||
					!assert.True(t, wantW.Code == http.StatusMovedPermanently || wantW.Code == http.StatusTemporaryRedirect,
						"Handler redirect status: table=%q request=%+v ServeMux %d", order, request, wantW.Code) {
					return
				}
				if !assert.Equal(t, target(wantW.Header().Get("Location")), target(w.Header().Get("Location")), "Handler redirect target: table=%q request=%+v", order, request) {
					return
				}
				assert.True(t, wantPattern == gotPattern || strings.HasPrefix(wantPattern, "/"),
					"Handler pattern: table=%q request=%+v", order, request)
				return
			}
			if !assert.Equal(t, wantPattern, gotPattern, "Handler pattern: table=%q request=%+v", order, request) {
				return
			}
			got = observedResponse{w.Code, w.Header(), w.Body.String(), r.Pattern, nil}
			want = observedResponse{wantW.Code, wantW.Header(), wantW.Body.String(), before.Pattern, nil}
			if !assert.Equal(t, want, got, "Handler response: table=%q request=%+v", order, request) {
				return
			}
		}
	}
}

func TestMuxDispatch(t *testing.T) {
	var pattern string
	m := NewMux()
	m.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		pattern = r.Pattern
		_, _ = w.Write([]byte("pong"))
	})

	w := serve(m, http.MethodGet, "/ping")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "pong", w.Body.String())
	assert.Equal(t, "GET /ping", pattern)
}

func TestMuxPathParam(t *testing.T) {
	m := NewMux()
	m.Get("/api/{uid}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("uid")))
	})

	w := serve(m, http.MethodGet, "/api/42")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "42", w.Body.String())
}

func TestMuxStaticWinsOverParam(t *testing.T) {
	m := NewMux()
	m.Get("/api/users", echo("users"))
	m.Get("/api/{id}", echo("id"))

	assert.Equal(t, "users", serve(m, http.MethodGet, "/api/users").Body.String())
	assert.Equal(t, "id", serve(m, http.MethodGet, "/api/42").Body.String())
}

func TestMuxManyParams(t *testing.T) {
	m := NewMux()
	pattern := "/{p1}/{p2}/{p3}/{p4}/{p5}/{p6}/{p7}/{p8}/{p9}/{p10}/{p11}/{p12}/{p13}/{p14}/{p15}/{p16}/{p17}"
	m.Get(pattern, func(w http.ResponseWriter, r *http.Request) {
		var values []string
		for i := 1; i <= 17; i++ {
			values = append(values, r.PathValue(fmt.Sprintf("p%d", i)))
		}
		_, _ = w.Write([]byte(strings.Join(values, ",")))
	})

	w := serve(m, http.MethodGet, "/1/2/3/4/5/6/7/8/9/10/11/12/13/14/15/16/17")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17", w.Body.String())
}

func TestMuxVerbs(t *testing.T) {
	tests := []struct {
		method   string
		register func(*Mux, string, func(http.ResponseWriter, *http.Request))
	}{
		{http.MethodGet, (*Mux).Get},
		{http.MethodHead, (*Mux).Head},
		{http.MethodPost, (*Mux).Post},
		{http.MethodPut, (*Mux).Put},
		{http.MethodPatch, (*Mux).Patch},
		{http.MethodDelete, (*Mux).Delete},
		{http.MethodConnect, (*Mux).Connect},
		{http.MethodOptions, (*Mux).Options},
		{http.MethodTrace, (*Mux).Trace},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			m := NewMux()
			test.register(m, "/ping", echo("pong"))

			assert.Equal(t, "pong", serve(m, test.method, "/ping").Body.String())
		})
	}

	m := NewMux()
	m.HandleFunc("GET /hi", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	})
	assert.Equal(t, "hi", serve(m, http.MethodGet, "/hi").Body.String())
}

func TestMuxTokenMethod(t *testing.T) {
	m := NewMux()
	m.Handle("V2 /ping", echo("v2"))

	assert.Equal(t, "v2", serve(m, "V2", "/ping").Body.String())
	assert.Equal(t, "V2", m.Routes()[0].Method)
}

func TestMuxAnyMethodPrecedence(t *testing.T) {
	for _, genericFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("genericFirst=%t", genericFirst), func(t *testing.T) {
			m := NewMux()
			if genericFirst {
				m.Handle("/ping", echo("generic"))
			}
			m.Get("/ping", echo("get"))
			if !genericFirst {
				m.Handle("/ping", echo("generic"))
			}
			assert.Equal(t, "get", serve(m, http.MethodGet, "/ping").Body.String())
			assert.Equal(t, "get", serve(m, http.MethodHead, "/ping").Body.String())
			assert.Equal(t, "generic", serve(m, http.MethodPost, "/ping").Body.String())
		})
	}
}

func TestMuxUseOrder(t *testing.T) {
	var order []string

	m := NewMux()
	m.Use(tracer("first", &order), tracer("second", &order))
	m.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	serve(m, http.MethodGet, "/ping")

	assert.Equal(t, []string{"first", "second", "handler"}, order)
}

func TestMuxUsePanics(t *testing.T) {
	m := NewMux()
	assert.PanicsWithError(t, "mino: Use middleware[0] is nil", func() {
		m.Use(nil)
	})

	m.Get("/ping", echo("pong"))
	assert.PanicsWithError(t, "mino: Use middleware[1] is nil", func() {
		m.Use(func(next http.Handler) http.Handler { return next }, nil)
	})
	assert.Empty(t, m.Middlewares(), "invalid Use must not append a partial stack")
	assert.Equal(t, "pong", serve(m, http.MethodGet, "/ping").Body.String())

	assert.PanicsWithError(t, "mino: middleware returned nil handler", func() {
		NewMux().Use(func(http.Handler) http.Handler { return nil })
	})
}

func TestMuxEmptyMiddleware(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			var calls []string
			m := NewMux()
			m.Use(tracer("root", &calls))
			var target Router = m
			path := "/missing"
			wantCalls := []string{"root"}
			if nested {
				target = m.Route("/api")
				target.Use(tracer("child", &calls))
				path = "/api/missing"
				wantCalls = append(wantCalls, "child")
			}

			for range 2 {
				calls = nil
				w := serve(m, http.MethodGet, path)
				assert.Equal(t, http.StatusNotFound, w.Code)
				assert.Equal(t, "404 page not found\n", w.Body.String())
				assert.Equal(t, wantCalls, calls)
			}
			// Resolve the current local or inherited fallback on every request.
			m.NotFound(echo("parent"))
			assert.Equal(t, "parent", serve(m, http.MethodGet, path).Body.String())
			target.NotFound(echo("local"))
			assert.Equal(t, "local", serve(m, http.MethodGet, path).Body.String())
			target.NotFound(nil)
			wantBody := "404 page not found\n"
			if nested {
				wantBody = "parent"
			}
			assert.Equal(t, wantBody, serve(m, http.MethodGet, path).Body.String())
		})
	}
}

func TestMuxEmptyMiddlewareConcurrent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%t", nested), func(t *testing.T) {
			var called atomic.Int32
			var built atomic.Int32
			mw := func(next http.Handler) http.Handler {
				built.Add(1)
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called.Add(1)
					next.ServeHTTP(w, r)
				})
			}
			m := NewMux()
			m.Use(mw)
			path := "/missing"
			layers := int32(1)
			if nested {
				m.Route("/api").Use(mw)
				path = "/api/missing"
				layers++
			}
			assert.Equal(t, layers, built.Load())
			var wg sync.WaitGroup
			for range 32 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w := serve(m, http.MethodGet, path)
					assert.Equal(t, http.StatusNotFound, w.Code)
				}()
			}
			wg.Wait()
			assert.Equal(t, 32*layers, called.Load())
			assert.Equal(t, layers, built.Load(), "concurrent requests must not rebuild middleware")
		})
	}
}

func TestMuxUseAfterRegistration(t *testing.T) {
	for name, configure := range map[string]func(*Mux) string{
		"Handle": func(m *Mux) string {
			m.Get("/ping", echo("pong"))
			return "/ping"
		},
		"With": func(m *Mux) string {
			m.With().Get("/ping", echo("pong"))
			return "/ping"
		},
		"Route": func(m *Mux) string {
			m.Route("/api").Get("/ping", echo("pong"))
			return "/api/ping"
		},
		"Mount": func(m *Mux) string {
			sub := NewMux()
			sub.Get("/ping", echo("pong"))
			m.Mount("/api", sub)
			return "/api/ping"
		},
		"host": func(m *Mux) string {
			m.Route("/api").Route("/v1").Get("example.com/ping", echo("pong"))
			return "http://example.com/api/v1/ping"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := NewMux()
			m.Use(marker("first"))
			path := configure(m)
			m.Use(marker("second"))
			for _, request := range []struct {
				method string
				path   string
				status int
			}{
				{http.MethodGet, path, http.StatusOK},
				{http.MethodPost, path, http.StatusMethodNotAllowed},
				{http.MethodGet, path + "/missing", http.StatusNotFound},
			} {
				w := serve(m, request.method, request.path)
				assert.Equal(t, request.status, w.Code)
				assert.Equal(t, []string{"first", "second"}, w.Header().Values("X-Chain"))
				if request.status == http.StatusMethodNotAllowed {
					assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
				}
			}
		})
	}
}

func TestMuxMiddlewareLifecycle(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, registered := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested=%t/registered=%t", nested, registered), func(t *testing.T) {
				m := NewMux()
				var target Router = m
				path := "/ping"
				if nested {
					target = m.Route("/api")
					path = "/api/ping"
				}
				builds := 0
				target.Use(func(next http.Handler) http.Handler {
					builds++
					requests := 0
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						w.Header().Set("X-Requests", fmt.Sprint(requests))
						next.ServeHTTP(w, r)
					})
				})
				assert.Equal(t, 1, builds, "Use builds middleware before any requests")
				target.Use()
				target.With()
				if registered {
					target.Get("/ping", echo("pong"))
				}
				target.NotFound(echo("fallback"))
				for i := 1; i <= 2; i++ {
					w := serve(m, http.MethodGet, path)
					assert.Equal(t, fmt.Sprint(i), w.Header().Get("X-Requests"))
					want := "fallback"
					if registered {
						want = "pong"
					}
					assert.Equal(t, want, w.Body.String())
				}
				assert.Equal(t, 1, builds, "registration and serving must reuse middleware state")
			})
		}
	}
}

func TestMuxWithMiddlewareSnapshots(t *testing.T) {
	m := NewMux()
	view := m.With(marker("view"))
	view.Get("/before", echo("before"))
	derived := view.With(marker("derived"))
	api := view.Route("/api")
	view.Mount("/tools", echo("mounted"))
	view.NotFound(echo("missing"))
	view.MethodNotAllowed(echo("wrong method"))

	view.Use(marker("later"))
	view.Get("/after", echo("after"))
	derived.Get("/derived", echo("derived"))
	api.Get("/ping", echo("pong"))
	api.Use(marker("api"))
	m.Use(marker("root"))
	m.Get("/public", echo("public"))

	for _, test := range []struct {
		method string
		path   string
		chain  []string
	}{
		{http.MethodGet, "/before", []string{"root", "view"}},
		{http.MethodGet, "/after", []string{"root", "view", "later"}},
		{http.MethodGet, "/derived", []string{"root", "view", "derived"}},
		{http.MethodGet, "/api/ping", []string{"root", "view", "api"}},
		{http.MethodGet, "/tools/ping", []string{"root", "view"}},
		{http.MethodGet, "/missing", []string{"root", "view"}},
		{http.MethodPost, "/before", []string{"root", "view"}},
		{http.MethodGet, "/public", []string{"root"}},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			w := serve(m, test.method, test.path)
			assert.Equal(t, test.chain, w.Header().Values("X-Chain"))
		})
	}
}

func TestMuxEntryBoundaries(t *testing.T) {
	m := NewMux()
	m.Use(marker("root"))
	m.NotFound(echo("parent 404"))
	m.Get("/api/public", echo("public"))
	api := m.Route("/api")
	api.Use(marker("api"))
	api.Get("/ping", echo("pong"))
	view := m.With(marker("inline"))
	view.Get("/secured", echo("secured"))
	sub := NewMux()
	sub.Use(marker("sub"))
	m.Mount("/sub", sub)

	for _, test := range []struct {
		name  string
		entry http.Handler
		path  string
		body  string
		chain []string
	}{
		{"root to child", m, "/api/ping", "pong", []string{"root", "api"}},
		{"direct child", api, "/api/ping", "pong", []string{"api"}},
		{"direct child inherits fallback", api, "/api/missing", "parent 404", []string{"api"}},
		{"parent endpoint wins", m, "/api/public", "public", []string{"root"}},
		{"inline endpoint", view, "/secured", "secured", []string{"root", "inline"}},
		{"inline is not an entry wrapper", view, "/outside", "parent 404", []string{"root"}},
		{"mount does not inherit fallback", m, "/sub/missing", "404 page not found\n", []string{"root", "sub"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			test.entry.ServeHTTP(w, httptest.NewRequest(http.MethodGet, test.path, nil))
			assert.Equal(t, test.body, w.Body.String())
			assert.Equal(t, test.chain, w.Header().Values("X-Chain"))
		})
	}
}

func TestMuxWithIsolation(t *testing.T) {
	var order []string

	m := NewMux()
	api := m.With(tracer("auth", &order))
	api.Get("/profile", echo("profile"))
	m.Get("/login", echo("login"))

	assert.Equal(t, "profile", serve(m, http.MethodGet, "/profile").Body.String())
	assert.Equal(t, []string{"auth"}, order)

	order = nil
	assert.Equal(t, "login", serve(m, http.MethodGet, "/login").Body.String())
	assert.Empty(t, order)
}

func TestMuxNestedWith(t *testing.T) {
	var order []string

	m := NewMux()
	api := m.With(tracer("auth", &order))
	deep := api.With(tracer("deep", &order))
	deep.Get("/deep", echo("deep"))
	m.Get("/plain", echo("plain"))

	assert.PanicsWithError(t, "mino: With middleware[0] is nil", func() {
		m.With(nil)
	})

	assert.Equal(t, "deep", serve(m, http.MethodGet, "/deep").Body.String())
	assert.Equal(t, []string{"auth", "deep"}, order)

	order = nil
	assert.Equal(t, "plain", serve(m, http.MethodGet, "/plain").Body.String())
	assert.Empty(t, order)
}

func TestMuxRouteOnInline(t *testing.T) {
	var order []string

	m := NewMux()
	api := m.With(tracer("auth", &order))
	g := api.Route("/g")
	g.Get("/ping", echo("grouped"))

	assert.Equal(t, "grouped", serve(m, http.MethodGet, "/g/ping").Body.String())
	assert.Equal(t, []string{"auth"}, order)
}

func TestMuxRoutePrefix(t *testing.T) {
	m := NewMux()
	assert.PanicsWithError(t, `mino: invalid prefix "": Route and Mount require a path beginning with '/'`, func() {
		m.Route("")
	})
	assert.PanicsWithError(t, `mino: invalid prefix "api": Route and Mount require a path beginning with '/'`, func() {
		m.Route("api")
	})
	assert.PanicsWithError(t, `mino: invalid prefix "/{a}": Route and Mount require a literal prefix without parameters`, func() {
		m.Route("/{a}")
	})

	g := m.Route("/api/")
	g.Get("/ping", echo("pong"))
	assert.Equal(t, "pong", serve(m, http.MethodGet, "/api/ping").Body.String())

	m.Route("//").Get("/ping", echo("pong"))
}

func TestMuxRouteGroup(t *testing.T) {
	m := NewMux()
	api := m.Route("/api")
	api.Get("/ping", echo("api pong"))
	m.Get("/ping", echo("pong"))

	assert.Equal(t, "api pong", serve(m, http.MethodGet, "/api/ping").Body.String())
	assert.Equal(t, "pong", serve(m, http.MethodGet, "/ping").Body.String())
}

func TestMuxRouteNested(t *testing.T) {
	m := NewMux()
	api := m.Route("/api")
	root := api.Route("/")
	root.Get("/ping", echo("root"))

	assert.Equal(t, "root", serve(m, http.MethodGet, "/api/ping").Body.String())
}

func TestMuxMount(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	})

	m := NewMux()
	m.Mount("/sub", inner)

	w := serve(m, http.MethodGet, "/sub/hello")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/hello", w.Body.String())

	assert.PanicsWithError(t, `mino: nil mount handler for prefix "/x"`, func() {
		m.Mount("/x", nil)
	})
}

func TestMuxMountPaths(t *testing.T) {
	writePath := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	})

	t.Run("root keeps full path", func(t *testing.T) {
		m := NewMux()
		m.Mount("/", writePath)

		assert.Equal(t, "/any", serve(m, http.MethodGet, "/any").Body.String())
		assert.Equal(t, "/", serve(m, http.MethodGet, "/").Body.String())
	})

	t.Run("nil handler func", func(t *testing.T) {
		m := NewMux()
		assert.PanicsWithError(t, `mino: nil mount handler for prefix "/x"`, func() {
			m.Mount("/x", http.HandlerFunc(nil))
		})
	})

	t.Run("raw path", func(t *testing.T) {
		m := NewMux()
		m.Mount("/sub", writePath)

		assert.Equal(t, "/", serve(m, http.MethodGet, "/sub").Body.String())
		assert.Equal(t, "/a", serve(m, http.MethodGet, "/sub%2Fa").Body.String())
		assert.Equal(t, "/x", serve(m, http.MethodGet, "/%73ub/x").Body.String())
	})
}

func TestMuxMountParameterPrecedence(t *testing.T) {
	for _, anyMethod := range []bool{false, true} {
		for _, mountFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("any=%t/mountFirst=%t", anyMethod, mountFirst), func(t *testing.T) {
				m := NewMux()
				sub := NewMux()
				sub.Get("/extra/info", echo("mounted"))
				if mountFirst {
					m.Mount("/api", sub)
				}
				if anyMethod {
					m.Handle("/api/{id}", echo("parameter"))
				} else {
					m.Get("/api/{id}", echo("parameter"))
				}
				if !mountFirst {
					m.Mount("/api", sub)
				}
				assert.Equal(t, "parameter", serve(m, http.MethodGet, "/api/42").Body.String())
				assert.Equal(t, "mounted", serve(m, http.MethodGet, "/api/extra/info").Body.String())
			})
		}
	}
}

func TestMountEncodedSlashRemainder(t *testing.T) {
	// A mount prefix ending right before an encoded slash must keep that
	// byte as the delegated path's leading separator, not segment data.
	child := NewMux()
	child.Get("/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.URL.Path, r.URL.RawPath)
	})
	m := NewMux()
	m.Mount("/p", child)

	w := serve(m, http.MethodGet, "/p%2Fx")

	assert.Equal(t, "/x|/x", w.Body.String())
}

func TestStripPrefix(t *testing.T) {
	h := stripPrefix("/sub", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sub/deep", nil))
	assert.Equal(t, "/deep", w.Body.String())

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/other", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStripPrefixEmptyRemainder(t *testing.T) {
	// RawPath equal to the prefix leaves an empty remainder; the
	// delegated path must still be "/".
	h := stripPrefix("/p", echo("child"))
	r := httptest.NewRequest(http.MethodGet, "/p", nil)
	r.URL.RawPath = "/p"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	assert.Equal(t, "child", w.Body.String())
}

func TestMuxHandleMethodPrefix(t *testing.T) {
	m := NewMux()
	m.Handle("POST /only", echo("posted"))

	assert.Equal(t, "posted", serve(m, http.MethodPost, "/only").Body.String())

	w := serve(m, http.MethodGet, "/only")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Equal(t, http.MethodPost, w.Header().Get("Allow"))
}

func TestMuxHandlePanics(t *testing.T) {
	m := NewMux()
	assert.PanicsWithError(t, `mino: invalid pattern "": a path beginning with '/' is required`, func() {
		m.Handle("", echo("x"))
	})
	assert.PanicsWithError(t, `mino: invalid HTTP method "{bad}" in pattern "{bad} /x"`, func() {
		m.Handle("{bad} /x", echo("x"))
	})
	assert.PanicsWithError(t, `mino: invalid pattern "noslash": a path beginning with '/' is required`, func() {
		m.Handle("noslash", echo("x"))
	})
	assert.PanicsWithError(t, `mino: invalid host "{bad}" in pattern "{bad}/x": host patterns must be literal`, func() {
		m.Handle("{bad}/x", echo("x"))
	})
	assert.PanicsWithError(t, `mino: nil handler for pattern "GET /x"`, func() {
		m.Handle("GET /x", nil)
	})
	assert.PanicsWithError(t, `mino: nil handler for pattern "GET /x"`, func() {
		m.HandleFunc("GET /x", nil)
	})
}

func TestMuxHandleRejectsUncleanPatternUnderPolicy(t *testing.T) {
	m := NewMux(WithCleanPathRedirect())

	assert.PanicsWithError(t,
		`mino: invalid pattern "GET //a": non-CONNECT path cannot match with clean-path redirects enabled`,
		func() { m.Handle("GET //a", echo("a")) })

	// CONNECT paths keep their authority spelling and stay exempt.
	assert.NotPanics(t, func() { m.Handle("CONNECT //a", echo("a")) })
}

func TestMuxConfOptions(t *testing.T) {
	t.Run("escaped path", func(t *testing.T) {
		m := NewMux(WithEscapedPathMatching())
		m.Get("/api/{uid}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.PathValue("uid")))
		})

		w := serve(m, http.MethodGet, "/api/a%2Fb")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "a/b", w.Body.String())
	})

	t.Run("escaped mount", func(t *testing.T) {
		m := NewMux(WithEscapedPathMatching())
		m.Mount("/sub", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.URL.Path))
		}))

		assert.Equal(t, "/x", serve(m, http.MethodGet, "/sub/x").Body.String())
	})

	t.Run("escaped redirect", func(t *testing.T) {
		m := NewMux(WithEscapedPathMatching(), WithTrailingSlashRedirect())
		m.Get("/a%20b/", echo("space"))

		w := serve(m, http.MethodGet, "/a%20b")
		assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
		assert.Equal(t, "/a%20b/", w.Header().Get("Location"))
	})

	t.Run("trailing slash on", func(t *testing.T) {
		m := NewMux(WithTrailingSlashRedirect())
		m.Get("/tree/", echo("tree"))

		w := serve(m, http.MethodGet, "/tree")
		assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
		assert.Equal(t, "/tree/", w.Header().Get("Location"))
	})

	t.Run("trailing slash off", func(t *testing.T) {
		m := NewMux()
		m.Get("/tree/", echo("tree"))

		assert.Equal(t, http.StatusNotFound, serve(m, http.MethodGet, "/tree").Code)
	})
}

func TestMuxServeHTTPFallback(t *testing.T) {
	w := recorder(NewMux(), httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var order []string
	m := NewMux()
	api := m.With(tracer("auth", &order))
	api.Get("/profile", echo("profile"))

	w = recorder(m, httptest.NewRequest(http.MethodGet, "/profile", nil))
	assert.Equal(t, "profile", w.Body.String())

	w = httptest.NewRecorder()
	order = nil
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/profile", nil))
	assert.Equal(t, "profile", w.Body.String())
	assert.Equal(t, []string{"auth"}, order)

	// 空路径的普通请求按 "/" 处理，CONNECT 保留空路径。
	w = recorder(m, httptest.NewRequest(http.MethodGet, "http://example.com", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)

	w = recorder(m, httptest.NewRequest(http.MethodConnect, "example.com:443", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestMuxHostRouting(t *testing.T) {
	m := NewMux()
	m.Handle("example.com/api", echo("host api"))
	m.Handle("POST example.com/only", echo("posted"))
	m.Get("api.example.com/ping", echo("host ping"))
	m.Get("/ping", echo("plain"))

	serveHost := func(host, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Host = host
		return recorder(m, r)
	}

	assert.Equal(t, "host ping", serveHost("api.example.com", "/ping").Body.String())
	assert.Equal(t, "host ping", serveHost("api.example.com:8443", "/ping").Body.String())
	assert.Equal(t, "host api", serveHost("example.com", "/api").Body.String())
	assert.Equal(t, "plain", serveHost("other.com", "/ping").Body.String())

	w := serveHost("example.com", "/only")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Equal(t, http.MethodPost, w.Header().Get("Allow"))

	var hosted []string
	for _, r := range m.Routes() {
		hosted = append(hosted, r.Host+" "+r.Method+" "+r.Pattern)
	}
	assert.Equal(t, []string{
		" GET /ping",
		"api.example.com GET /ping",
		"example.com  /api",
		"example.com POST /only",
	}, hosted)

	t.Run("nested", func(t *testing.T) {
		m := NewMux()
		api := m.Route("/api")
		api.Handle("example.com/ping", echo("deep"))

		r := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		r.Host = "example.com"

		assert.Equal(t, "deep", recorder(m, r).Body.String())
	})
}

func TestMuxNotFound(t *testing.T) {
	m := NewMux()
	m.Get("/ping", echo("pong"))

	assert.Equal(t, http.StatusNotFound, serve(m, http.MethodGet, "/missing").Code)

	m.NotFound(echo("custom 404"))
	assert.Equal(t, "custom 404", serve(m, http.MethodGet, "/missing").Body.String())
}

func TestMuxFallbacksInline(t *testing.T) {
	m := NewMux()
	api := m.With(func(next http.Handler) http.Handler { return next })
	api.NotFound(nil)
	api.NotFound(echo("api 404"))
	api.MethodNotAllowed(nil)
	api.MethodNotAllowed(echo("api 405"))
	m.Get("/ping", echo("pong"))

	assert.Equal(t, "api 404", serve(m, http.MethodGet, "/missing").Body.String())
	assert.Equal(t, "api 405", serve(m, http.MethodPost, "/ping").Body.String())
}

func TestMuxFallbackReset(t *testing.T) {
	fallbacks := []struct {
		name   string
		set    func(Router, http.HandlerFunc)
		method string
		path   string
		status int
		body   string
	}{
		{"NotFound", Router.NotFound, http.MethodGet, "/missing", http.StatusNotFound, "404 page not found\n"},
		{"MethodNotAllowed", Router.MethodNotAllowed, http.MethodPost, "/ping", http.StatusMethodNotAllowed, "Method Not Allowed\n"},
	}
	for _, fallback := range fallbacks {
		for _, mounted := range []bool{false, true} {
			for _, depth := range []int{0, 1, 2} {
				t.Run(fmt.Sprintf("%s/mounted=%t/inline=%d", fallback.name, mounted, depth), func(t *testing.T) {
					var calls []string
					handler := func(name string) http.HandlerFunc {
						return func(w http.ResponseWriter, r *http.Request) {
							calls = append(calls, name)
							w.WriteHeader(fallback.status)
							_, _ = w.Write([]byte(name))
						}
					}

					m := NewMux()
					m.Use(tracer("root", &calls))
					var owner Router = m
					path := fallback.path
					baseCalls := []string{"root"}
					wantBody := fallback.body
					if mounted {
						fallback.set(m, handler("inherited"))
						owner = m.Route("/api")
						owner.Use(tracer("child", &calls))
						path = "/api" + path
						baseCalls = append(baseCalls, "child")
						wantBody = "inherited"
					}
					owner.Get("/ping", echo("pong"))

					target := owner
					customCalls := append([]string(nil), baseCalls...)
					for i := range depth {
						name := fmt.Sprintf("inline%d", i)
						target = target.With(tracer(name, &calls))
						customCalls = append(customCalls, name)
					}
					fallback.set(target, handler("custom"))
					w := serve(m, fallback.method, path)
					assert.Equal(t, fallback.status, w.Code)
					assert.Equal(t, "custom", w.Body.String())
					assert.Equal(t, append(customCalls, "custom"), calls)

					// Repeated resets restore inheritance or the default response
					// without keeping the inline middleware wrapper.
					fallback.set(target, nil)
					fallback.set(target, nil)
					calls = nil
					w = serve(m, fallback.method, path)
					assert.Equal(t, fallback.status, w.Code)
					assert.Equal(t, wantBody, w.Body.String())
					wantCalls := append([]string(nil), baseCalls...)
					if mounted {
						wantCalls = append(wantCalls, "inherited")
					}
					assert.Equal(t, wantCalls, calls)
					if fallback.status == http.StatusMethodNotAllowed {
						assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
					}
				})
			}
		}
	}
}

func TestMuxMethodNotAllowed(t *testing.T) {
	m := NewMux()
	m.Get("/ping", echo("pong"))

	w := serve(m, http.MethodPost, "/ping")
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))

	m.MethodNotAllowed(echo("custom 405"))
	assert.Equal(t, "custom 405", serve(m, http.MethodPost, "/ping").Body.String())
}

func TestMuxHeadFallback(t *testing.T) {
	m := NewMux()
	m.Get("/ping", echo("pong"))

	assert.Equal(t, http.StatusOK, serve(m, http.MethodHead, "/ping").Code)
}

func TestMuxTrailingSlashRedirect(t *testing.T) {
	m := NewMux(WithTrailingSlashRedirect())
	m.Get("/tree/", echo("tree root"))

	w := serve(m, http.MethodGet, "/tree")
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "/tree/", w.Header().Get("Location"))
}

func TestMuxRedirectStatus(t *testing.T) {
	m := NewMux(WithTrailingSlashRedirect())
	m.Get("/tree/", echo("tree"))
	m.Post("/tree/", echo("tree"))

	w := serve(m, http.MethodPost, "/tree")
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "/tree/", w.Header().Get("Location"))

	w = serve(m, http.MethodGet, "/tree?x=1")
	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "/tree/?x=1", w.Header().Get("Location"))
}

func TestMuxCleanPathRedirect(t *testing.T) {
	m := NewMux(WithCleanPathRedirect())
	m.Get("/api/ping", echo("pong"))

	r := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	r.URL.Path = "//api/ping"
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)

	assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
	assert.Equal(t, "/api/ping", w.Header().Get("Location"))
}

// TestMuxCleanRedirectEscaping pins Location escaping: the cleaned escaped
// path goes out verbatim at any escape depth, matching Go 1.27's ServeMux.
func TestMuxCleanRedirectEscaping(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		target   string
		want     string
	}{
		{"plain double slash", []string{"GET /a/b"}, "/a//b", "/a/b"},
		{"dot segment", []string{"GET /a/b"}, "/a/./b", "/a/b"},
		{"encoded slash", []string{"GET /a%2Fb/c"}, "/a%2Fb//c?q=%25", "/a%2Fb/c?q=%25"},
		{"double encoded slash", []string{"GET /a%2Fb/c"}, "/a%252Fb//c", "/a%252Fb/c"},
		{"triple encoded slash", []string{"GET /a%2Fb/c"}, "/a%25252Fb//c", "/a%25252Fb/c"},
		{"encoded alpha stays data", []string{"GET /"}, "/a%61//c", "/a%61/c"},
		{"query kept verbatim", []string{"GET /a/b"}, "/a//b?x=%252F", "/a/b?x=%252F"},
		{"encoded dots stay data", []string{"GET /"}, "/a/%2e%2e//b", "/a/%2e%2e/b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux(WithCleanPathRedirect())
			for _, pattern := range test.patterns {
				m.Handle(pattern, echo(pattern))
			}

			w := serve(m, http.MethodGet, test.target)
			assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
			assert.Equal(t, test.want, w.Header().Get("Location"))
		})
	}
}

// TestRedirectURL pins the URL construction contract: a valid escaping is
// preserved verbatim through RawPath; an invalid triplet falls back to
// re-encoding, unreachable for requests whose EscapedPath is always valid.
func TestRedirectURL(t *testing.T) {
	u := redirectURL("/a%2Fb/c")
	assert.Equal(t, "/a/b/c", u.Path)
	assert.Equal(t, "/a%2Fb/c", u.RawPath)
	assert.Equal(t, "/a%2Fb/c", u.String())

	bad := redirectURL("/a%zz")
	assert.Equal(t, "/a%zz", bad.Path)
	assert.Equal(t, "/a%25zz", bad.String())
}

func TestRedirectStatus(t *testing.T) {
	for _, code := range []int{0, 301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			want := code
			if code == 0 {
				want = http.StatusTemporaryRedirect
			}
			assert.Equal(t, want, redirectStatus(code))
		})
	}
	for _, code := range []int{-1, 200, 300, 304, 305, 306, 309, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			assert.PanicsWithError(t, fmt.Sprintf("mino: invalid redirect status %d: use 301, 302, 303, 307 or 308", code), func() {
				redirectStatus(code)
			})
		})
	}
}

func TestRedirectPath(t *testing.T) {
	tests := []struct {
		name   string
		target url.URL
		query  string
		want   string
	}{
		{"plain", url.URL{Path: "/docs/"}, "", "/docs/"},
		{"encoded path", url.URL{Path: "/files/a/b/%2F/", RawPath: "/files/a%2fb/%252F/"}, "q=a%2Fb&x=%25", "/files/a%2fb/%252F/?q=a%2Fb&x=%25"},
		{"cleaned escaped path", url.URL{Path: "/a%2Fb/c"}, "q=%25", "/a%252Fb/c?q=%25"},
		{"keep unclean path", url.URL{Path: "/a//b/./c/"}, "q=1", "/a//b/./c/?q=1"},
		{"same origin", url.URL{Path: "//other.example/docs/"}, "", "/.//other.example/docs/"},
		{"encoded leading slash", url.URL{Path: "//other.example/", RawPath: "/%2Fother.example/"}, "", "/%2Fother.example/"},
		{"unicode query", url.URL{Path: "/docs/"}, "q=海诺&x=%25", "/docs/?q=%e6%b5%b7%e8%af%ba&x=%25"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux()
			r := httptest.NewRequest(http.MethodHead, "http://origin.example/start", nil)
			r.URL.RawQuery = test.query
			w := httptest.NewRecorder()

			m.writeRedirect(w, r, redirectLocation(test.target, r.URL.RawQuery))

			assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
			assert.Equal(t, test.want, w.Header().Get("Location"))
			location, err := url.Parse(w.Header().Get("Location"))
			if assert.NoError(t, err) {
				assert.Equal(t, r.URL.Host, r.URL.ResolveReference(location).Host)
			}
		})
	}
}

func TestRedirectPathResponse(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		code        int
		header      http.Header
		contentType string
		body        string
	}{
		{name: "get default", method: http.MethodGet, contentType: "text/html; charset=utf-8", body: "<a href=\"/docs/?a=1&amp;b=%25\">Temporary Redirect</a>.\n\n"},
		{name: "head", method: http.MethodHead, code: 301, contentType: "text/html; charset=utf-8"},
		{name: "post", method: http.MethodPost, code: 307},
		{name: "custom status", method: http.MethodGet, code: 308, contentType: "text/html; charset=utf-8", body: "<a href=\"/docs/?a=1&amp;b=%25\">Permanent Redirect</a>.\n\n"},
		{name: "existing content type", method: http.MethodGet, code: 302, header: http.Header{"Content-Type": {"application/json"}}, contentType: "application/json"},
		{name: "suppressed content type", method: http.MethodGet, code: 303, header: http.Header{"Content-Type": nil}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux(WithRedirectStatus(test.code))
			r := httptest.NewRequest(test.method, "/start?a=1&b=%25", nil)
			w := httptest.NewRecorder()
			for key, values := range test.header {
				w.Header()[key] = values
			}

			m.writeRedirect(w, r, redirectLocation(url.URL{Path: "/docs/"}, r.URL.RawQuery))

			code := test.code
			if code == 0 {
				code = http.StatusTemporaryRedirect
			}
			assert.Equal(t, code, w.Code)
			assert.Equal(t, "/docs/?a=1&b=%25", w.Header().Get("Location"))
			assert.Equal(t, test.contentType, w.Header().Get("Content-Type"))
			assert.Equal(t, test.body, w.Body.String())
		})
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "/"},
		{"root", "/", "/"},
		{"relative", "api/x", "/api/x"},
		{"collapse", "/a//b/./c", "/a/b/c"},
		{"parent", "/a/b/../c", "/a/c"},
		{"above root", "/../../a", "/a"},
		{"keep trailing", "/a/b/", "/a/b/"},
		{"collapsed trailing", "/a//b/../", "/a/"},
		{"collapsed root", "/a/../", "/"},
		{"encoded slash", "/a%2Fb//c", "/a%2Fb/c"},
		{"encoded dots", "/a/%2e/%2E%2e/b", "/a/%2e/%2E%2e/b"},
		{"encoded percent", "/a/%252e%252e//b", "/a/%252e%252e/b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cleanPath(test.in))
		})
	}
}

func TestEscapedMatchPath(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		isPattern bool
		want      string
	}{
		{name: "empty"},
		{name: "plain", in: "/api/users", want: "/api/users"},
		{name: "equivalent escape", in: "/%61pi/%75sers", want: "/api/users"},
		{name: "encoded slash", in: "/files/a%2fb", want: "/files/a%2Fb"},
		{name: "literal slash", in: "/files/a/b", want: "/files/a/b"},
		{name: "single decode", in: "/files/a%252Fb", want: "/files/a%252Fb"},
		{name: "literal braces", in: "/files/%7bid%7d", want: "/files/%7Bid}"},
		{name: "structural bytes", in: "/%2f%25%7b", want: "/%2F%25%7B"},
		{name: "unicode", in: "/users/%E6%B5%B7%E8%AF%BA", want: "/users/海诺"},
		{name: "keep separators", in: "/%61pi//users/", want: "/api//users/"},
		{name: "parameter pattern", in: "/%61pi/{id}", isPattern: true, want: "/api/{id}"},
		{name: "catch-all pattern", in: "/%66iles/{rest...}", isPattern: true, want: "/files/{rest...}"},
		{name: "exact slash pattern", in: "/%64ocs/{$}", isPattern: true, want: "/docs/{$}"},
		{name: "static pattern", in: "/%61pi/%7bid%7d", isPattern: true, want: "/api/%7Bid}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, escapedMatchPath(test.in, test.isPattern))
		})
	}
}

func TestUnescapePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "/api/users", "/api/users"},
		{"slash", "a%2Fb", "a/b"},
		{"single decode", "a%252Fb", "a%2Fb"},
		{"plus is literal", "a+b%20c", "a+b c"},
		{"unicode", "%E6%B5%B7%E8%AF%BA", "海诺"},
		{"incomplete escape", "a%2", "a%2"},
		{"invalid escape", "a%20b/%zz", "a%20b/%zz"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, unescapePath(test.in))
		})
	}
}

func TestEscapedPrefixEnd(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		prefix  string
		escaped string
	}{
		{"empty", "/api/users", "", ""},
		{"plain", "/api/users", "/api", "/api"},
		{"encoded ascii", "/%61pi/users", "/api", "/%61pi"},
		{"encoded slash", "/api/a%2fb/next", "/api/a/b", "/api/a%2fb"},
		{"unicode", "/%E4%BD%A0%E5%A5%BD/users", "/你好", "/%E4%BD%A0%E5%A5%BD"},
		{"mixed spelling", "/你%E5%A5%BD/users", "/你好", "/你%E5%A5%BD"},
		{"single decode", "/a%252Fb/users", "/a%2Fb", "/a%252Fb"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			end := escapedPrefixEnd(test.in, len(test.prefix))
			assert.Equal(t, test.escaped, test.in[:end])
			assert.Equal(t, test.prefix, unescapePath(test.in[:end]))
		})
	}
}

func TestEscapeNonASCII(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii", "/docs/?q=a%2Fb&x=%25", "/docs/?q=a%2Fb&x=%25"},
		{"unicode", "/docs/?q=海诺", "/docs/?q=%e6%b5%b7%e8%af%ba"},
		{"mixed spelling", "/你?q=好&x=%E5%A5%BD", "/%e4%bd%a0?q=%e5%a5%bd&x=%E5%A5%BD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, escapeNonASCII(test.in))
		})
	}
}

func TestMuxEscapedTerminalSlash(t *testing.T) {
	for _, pattern := range []string{"GET /{$}", "GET /%2f", "GET /a/{$}", "GET /a/%2f", "GET /a/{id}/{$}", "GET /a/{id}/%2f"} {
		t.Run(pattern, func(t *testing.T) {
			var requests []requestSpec
			for _, target := range []string{"/", "/%2F", "/%2f", "/%252F", "/a/", "/a/%2F", "/a/%2f", "/a/%252F", "/a/%2F/", "/a/42/", "/a/42/%2F", "/a/42/%252F", "/a/42/%2F/extra"} {
				for _, method := range []string{"GET", "HEAD", "POST"} {
					requests = append(requests, requestSpec{method, target, ""})
				}
			}
			checkLegalRouteTable(t, []string{pattern}, requests)
		})
	}
}

func TestMuxAsteriskRequestURI(t *testing.T) {
	m := NewMux()
	m.Get("/ping", echo("pong"))

	r := httptest.NewRequest(http.MethodOptions, "http://example.com/", nil)
	r.RequestURI = "*"

	w := recorder(m, r)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "close", w.Header().Get("Connection"))
}

func TestMuxRoutesSorted(t *testing.T) {
	m := NewMux()
	m.Post("/b", echo("b"))
	m.Get("/a", echo("a"))
	m.Get("/c", echo("c"))

	var patterns []string
	for _, r := range m.Routes() {
		patterns = append(patterns, r.Method+" "+r.Pattern)
	}

	assert.Equal(t, []string{"GET /a", "GET /c", "POST /b"}, patterns)
}

func TestMuxMiddlewaresCopy(t *testing.T) {
	m := NewMux()
	m.Use(func(next http.Handler) http.Handler { return next })

	stack := m.Middlewares()
	assert.Len(t, stack, 1)

	stack[0] = nil
	assert.NotNil(t, m.Middlewares()[0])
}

func TestClearRouteValues(t *testing.T) {
	// Clearing removes only the parameters named by a previous Mino match,
	// leaving application-owned values installed by middleware untouched.
	r := httptest.NewRequest(http.MethodGet, "/a/42/b", nil)
	r.SetPathValue("id", "42")
	r.SetPathValue("name", "n")
	r.SetPathValue("custom", "keep")
	r.Pattern = "GET /a/{id}/b/{name}"

	clearRouteValues(r)

	assert.Empty(t, r.PathValue("id"))
	assert.Empty(t, r.PathValue("name"))
	assert.Equal(t, "keep", r.PathValue("custom"))

	// Catch-all parameters clear too.
	r = httptest.NewRequest(http.MethodGet, "/a/x/y", nil)
	r.SetPathValue("rest", "x/y")
	r.Pattern = "GET /a/{rest...}"
	clearRouteValues(r)
	assert.Empty(t, r.PathValue("rest"))

	// "{$}" is not a parameter name; an unclosed brace must not loop forever.
	r.Pattern = "GET /{$}"
	clearRouteValues(r)
	r.Pattern = "GET /a/{unclosed"
	clearRouteValues(r)
}

func TestMuxRematchClearsStaleValues(t *testing.T) {
	// Re-dispatching a request another Mux already matched must clear
	// stale parameters and install only the new match's values.
	var carried *http.Request
	first := NewMux()
	first.Get("/api/{id}", func(w http.ResponseWriter, r *http.Request) {
		carried = r.Clone(r.Context())
	})
	recorder(first, httptest.NewRequest(http.MethodGet, "/api/42", nil))

	second := NewMux()
	second.Get("/api/{name}", echo("second"))
	w := recorder(second, carried)

	assert.Equal(t, "second", w.Body.String())
	assert.Equal(t, "GET /api/{name}", carried.Pattern)
	assert.Empty(t, carried.PathValue("id"))
	assert.Equal(t, "42", carried.PathValue("name"))
}

func TestValidMethod(t *testing.T) {
	tests := []struct {
		name   string
		method string
		valid  bool
	}{
		{"empty", "", false},
		{"letters", "GET", true},
		{"digits", "V2", true},
		{"token chars", "a!#$%&'*+-.^_`|~", true},
		{"brace", "{bad}", false},
		{"space", "a b", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.valid, validMethod(test.method))
		})
	}
}

func TestRequestHost(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		method string
		want   string
	}{
		{"no port", "example.com", http.MethodGet, "example.com"},
		{"with port", "example.com:8443", http.MethodGet, "example.com"},
		{"connect keeps raw", "example.com:8443", http.MethodConnect, "example.com:8443"},
		{"unparsable keeps raw", "a:b:c", http.MethodGet, "a:b:c"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := &http.Request{Host: test.host, Method: test.method}
			assert.Equal(t, test.want, requestHost(r))
		})
	}
}

func TestChainHandler(t *testing.T) {
	var order []string

	h := &ChainHandler{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "handler")
		}),
	}
	h.compile([]chain.Middleware{tracer("first", &order), tracer("second", &order)})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"first", "second", "handler"}, order)
}

func TestChainHandlerCompile(t *testing.T) {
	stamp := func(name string) chain.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Middleware", name)
				next.ServeHTTP(w, r)
			})
		}
	}

	mws := []chain.Middleware{stamp("first")}
	h := &ChainHandler{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("finish"))
		}),
	}
	h.compile(mws)

	mws[0] = stamp("hacked")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "first", w.Header().Get("X-Middleware"))
	assert.Equal(t, "finish", w.Body.String())
}

func TestMuxHandlerCompatibility(t *testing.T) {
	tests := []struct {
		name                 string
		patterns             []string
		method, target, host string
	}{
		{name: "static", patterns: []string{"GET /hello"}, method: "GET", target: "/hello"},
		{name: "parameter", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/42"},
		{name: "head fallback", patterns: []string{"GET /hello"}, method: "HEAD", target: "/hello"},
		{name: "explicit head", patterns: []string{"GET /hello", "HEAD /hello"}, method: "HEAD", target: "/hello"},
		{name: "any method", patterns: []string{"GET /hello", "/hello"}, method: "POST", target: "/hello"},
		{name: "host port", patterns: []string{"GET example.com/hello", "GET /hello"}, method: "GET", target: "http://example.com:8080/hello"},
		{name: "not found", patterns: []string{"GET /hello"}, method: "GET", target: "/missing"},
		{name: "method not allowed", patterns: []string{"GET /hello", "POST /hello"}, method: "PUT", target: "/hello"},
		{name: "empty", method: "GET", target: "/missing"},
		{name: "encoded parameter", patterns: []string{"GET /files/{id}"}, method: "GET", target: "/files/a%2Fb"},
		{name: "encoded slash", patterns: []string{"GET /files/{id}"}, method: "GET", target: "/files/%2f"},
		{name: "encoded fallback", patterns: []string{"GET /files/{id}", "GET /files/{rest...}"}, method: "GET", target: "/files/%2F"},
		{name: "single decode", patterns: []string{"GET /files/{id}"}, method: "GET", target: "/files/%252F"},
		{name: "unicode static", patterns: []string{"GET /你好"}, method: "GET", target: "/%E4%BD%A0%E5%A5%BD"},
		{name: "equivalent escape", patterns: []string{"GET /hello"}, method: "GET", target: "/%68ello"},
		{name: "catch all", patterns: []string{"GET /files/{rest...}"}, method: "GET", target: "/files/a/b"},
		{name: "exact slash", patterns: []string{"GET /docs/{$}"}, method: "GET", target: "/docs"},
		{name: "slash redirect", patterns: []string{"GET /docs/"}, method: "GET", target: "/docs?q=%25"},
		{name: "post slash redirect", patterns: []string{"POST /docs/"}, method: "POST", target: "/docs?q=1"},
		{name: "explicit root", patterns: []string{"GET /docs", "GET /docs/"}, method: "GET", target: "/docs"},
		{name: "clean redirect", patterns: []string{"GET /hello"}, method: "GET", target: "/a/../hello?q=1"},
		{name: "encoded clean redirect", patterns: []string{"GET /a%2Fb/c"}, method: "GET", target: "/a%2Fb//c?q=%25"},
		{name: "combined redirect", patterns: []string{"GET /a%2Fb/docs/"}, method: "GET", target: "/a%2Fb//docs?q=1"},
		{name: "empty clean redirect", method: "HEAD", target: "/a//b?q=1"},
		{name: "connect dispatch host", patterns: []string{"CONNECT dispatch.example/docs/{id}"}, method: "CONNECT", target: "http://probe.example/docs/42", host: "dispatch.example"},
		{name: "connect redirect", patterns: []string{"CONNECT probe.example/docs/", "CONNECT dispatch.example/docs"}, method: "CONNECT", target: "http://probe.example/docs", host: "dispatch.example"},
		{name: "connect allow", patterns: []string{"CONNECT probe.example/docs", "CONNECT dispatch.example/docs/"}, method: "CONNECT", target: "http://probe.example/docs", host: "dispatch.example"},
		{name: "connect unclean target", patterns: []string{"CONNECT probe.example/a/../docs/"}, method: "CONNECT", target: "http://probe.example/a/../docs?q=%25", host: "dispatch.example"},
		{name: "asterisk query", method: "OPTIONS", target: "*"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching(), WithTrailingSlashRedirect())
			standard := http.NewServeMux()
			for _, pattern := range test.patterns {
				// Identify the selected entry without relying on dispatch metadata.
				h := echo(pattern)
				m.Handle(pattern, h)
				standard.Handle(pattern, h)
			}
			r := httptest.NewRequest(http.MethodGet, test.target, nil)
			r.Method = test.method
			if test.host != "" {
				r.Host = test.host
			}
			r.Pattern = "previous"
			r.SetPathValue("id", "existing")
			r.SetPathValue("application", "retained")
			r = r.WithContext(context.WithValue(r.Context(), handlerContextKey{}, "context"))
			before := r.Clone(r.Context())
			originalURL := r.URL
			var router Router = m
			gotHandler, gotPattern := router.Handler(r)
			wantHandler, wantPattern := standard.Handler(before)
			assert.NotNil(t, gotHandler)
			assert.Equal(t, before, r, "lookup must preserve the entire request")
			assert.Same(t, originalURL, r.URL)
			assert.Equal(t, "existing", r.PathValue("id"))
			w, wantW := httptest.NewRecorder(), httptest.NewRecorder()
			gotHandler.ServeHTTP(w, r.Clone(r.Context()))
			wantHandler.ServeHTTP(wantW, before.Clone(before.Context()))
			redirected := w.Code == http.StatusMovedPermanently || w.Code == http.StatusTemporaryRedirect ||
				wantW.Code == http.StatusMovedPermanently || wantW.Code == http.StatusTemporaryRedirect
			if redirected {
				// ServeMux's automatic redirect output drifts across
				// 1.23–1.27 (307 since 1.26, matched-pattern responses
				// since 1.25, request-escaped Location since 1.27):
				// require the same redirect to the same twice-decoded
				// target; spelling is pinned by mino's own redirect tests.
				target := func(location string) string {
					path, query, hasQuery := strings.Cut(location, "?")
					for {
						decoded := unescapePath(path)
						if decoded == path {
							break
						}
						path = decoded
					}
					if hasQuery {
						return path + "?" + query
					}
					return path
				}
				assert.Equal(t, http.StatusTemporaryRedirect, w.Code, "redirect status: mino")
				assert.True(t, wantW.Code == http.StatusMovedPermanently || wantW.Code == http.StatusTemporaryRedirect,
					"redirect status: ServeMux %d", wantW.Code)
				assert.Equal(t, target(wantW.Header().Get("Location")), target(w.Header().Get("Location")), "redirect target")
				assert.Equal(t, wantW.Header()["Content-Type"], w.Header()["Content-Type"], "redirect content type")
				assert.True(t, wantPattern == gotPattern || strings.HasPrefix(wantPattern, "/"),
					"redirect pattern: mino %q, ServeMux %q", gotPattern, wantPattern)
				return
			}
			assert.Equal(t, wantPattern, gotPattern)
			assert.Equal(t, wantW.Code, w.Code)
			assert.Equal(t, wantW.Header(), w.Header())
			assert.Equal(t, wantW.Body.String(), w.Body.String())
		})
	}
}

func ExampleMux_Handler() {
	m := NewMux()
	api := m.Route("/api")
	api.Get("/users/{id}", func(http.ResponseWriter, *http.Request) {})
	r := httptest.NewRequest("GET", "/api/users/42", nil)
	_, pattern := api.Handler(r)
	fmt.Println(pattern)
	fmt.Println(r.PathValue("id") == "")
	// Output:
	// GET /api/users/{id}
	// true
}

func TestMuxHandlerParameters(t *testing.T) {
	m := NewMux()
	m.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("id")))
	})
	r := httptest.NewRequest("GET", "/users/42", nil)
	h, pattern := m.Handler(r)
	assert.Equal(t, "GET /users/{id}", pattern)
	assert.Empty(t, r.Pattern)
	assert.Empty(t, r.PathValue("id"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Empty(t, w.Body.String(), "the registered handler does not install parameters")
	assert.Equal(t, "42", serve(m, "GET", "/users/42").Body.String())
}

func TestMuxHandlerTreatsEmptyPathAsRoot(t *testing.T) {
	m := NewMux()
	m.Get("/", echo("root"))

	// Hand-built requests may carry an empty URL.Path; matching must
	// normalize it to "/" exactly like ServeHTTP does.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.URL.Path = ""

	h, pattern := m.Handler(r)
	assert.Equal(t, "GET /", pattern)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, "root", w.Body.String())
}

func TestMuxHandlerPathPolicies(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("policies=%d", mask), func(t *testing.T) {
			m := NewMuxWithConf(MuxConf{
				CleanPathRedirect: mask&1 != 0, EscapedPath: mask&2 != 0,
				TrailingSlashRedirect: mask&4 != 0,
			})
			m.Get("/files/{id}", echo("parameter"))
			m.Get("/docs/", echo("docs"))
			m.Get("/a%2Fb/docs/", echo("encoded docs"))
			for _, target := range []string{"/files/42", "/files/a%2Fb", "/files/%2F", "/files/%252F", "/docs", "/docs/", "/a//docs?q=%25", "/a%2Fb//docs?q=1", "/missing"} {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
					t.Run(method+" "+target, func(t *testing.T) {
						r := httptest.NewRequest(method, target, nil)
						h, _ := m.Handler(r)
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						wantW := serve(m, method, target)
						assert.Equal(t, wantW.Code, w.Code)
						assert.Equal(t, wantW.Header(), w.Header())
						assert.Equal(t, wantW.Body.String(), w.Body.String())
					})
				}
			}
		})
	}
}

func TestMuxHandlerViews(t *testing.T) {
	var calls []string
	m := NewMux()
	m.Use(tracer("root", &calls))
	m.Get("/public", func(http.ResponseWriter, *http.Request) { calls = append(calls, "public") })
	view := m.With(tracer("inline", &calls))
	view.Get("/private", func(http.ResponseWriter, *http.Request) { calls = append(calls, "private") })
	api := view.Route("/api")
	api.Use(tracer("child", &calls))
	api.Get("/ping", func(http.ResponseWriter, *http.Request) { calls = append(calls, "ping") })
	view.Mount("/external", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
	}))

	tests := []struct {
		name, target, pattern string
		router                Router
		calls                 []string
	}{
		{"root excludes Use", "/public", "GET /public", m, []string{"public"}},
		{"view queries owner", "/public", "GET /public", view, []string{"public"}},
		{"bound inline retained", "/private", "GET /private", m, []string{"inline", "private"}},
		{"view does not double inline", "/private", "GET /private", view, []string{"inline", "private"}},
		{"route full prefix", "/api/ping", "GET /api/ping", api, []string{"ping"}},
		{"route relative misses", "/ping", "", api, nil},
		{"route entry remains delegate", "/api/ping", "/api/", m, []string{"inline", "child", "ping"}},
		{"mount keeps strip prefix", "/external/file", "/external/", m, []string{"inline", "/file"}},
		{"mount root", "/external", "/external", view, []string{"inline", "/"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls = nil
			r := httptest.NewRequest(http.MethodGet, test.target, nil)
			h, pattern := test.router.Handler(r)
			assert.Empty(t, calls, "lookup must not execute middleware or handlers")
			assert.Empty(t, r.Pattern)
			assert.Equal(t, test.pattern, pattern)
			h.ServeHTTP(httptest.NewRecorder(), r)
			assert.Equal(t, test.calls, calls)
		})
	}
}

func TestMuxHandlerFallbacks(t *testing.T) {
	var calls []string
	m := NewMux()
	m.Use(tracer("root", &calls))
	view := m.With(tracer("fallback", &calls))
	view.NotFound(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "not found")
		w.WriteHeader(http.StatusNotFound)
	})
	view.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "not allowed")
		assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	api := m.Route("/api")
	api.Use(tracer("child", &calls))
	api.Get("/ping", echo("ping"))
	for _, test := range []struct {
		method, target string
		status         int
		calls          []string
	}{
		{"GET", "/api/missing", http.StatusNotFound, []string{"fallback", "not found"}},
		{"POST", "/api/ping", http.StatusMethodNotAllowed, []string{"fallback", "not allowed"}},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			calls = nil
			r := httptest.NewRequest(test.method, test.target, nil)
			h, pattern := api.Handler(r)
			assert.Empty(t, calls)
			assert.Empty(t, pattern)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assert.Equal(t, test.status, w.Code)
			assert.Equal(t, test.calls, calls)
		})
	}
	api.NotFound(echo("local"))
	h, _ := api.Handler(httptest.NewRequest("GET", "/api/missing", nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/missing", nil))
	assert.Equal(t, "local", w.Body.String())
	api.NotFound(nil)
	calls = nil
	h, _ = api.With().Handler(httptest.NewRequest("GET", "/api/missing", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/missing", nil))
	assert.Equal(t, []string{"fallback", "not found"}, calls)
}

func TestMuxEmptyMuxClearsDelegatedMetadata(t *testing.T) {
	m := NewMux()
	m.NotFound(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%t|%q|%s", r.Pattern == "", r.PathValue("id"), r.PathValue("custom"))
	})

	// A request an outer Mux already matched re-enters an empty mux: the
	// delegated pattern and parameters clear; application values survive.
	r := httptest.NewRequest(http.MethodGet, "/missing", nil)
	r.SetPathValue("id", "42")
	r.SetPathValue("custom", "keep")
	r.Pattern = "GET /{id}"

	w := recorder(m, r)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "true|\"\"|keep", w.Body.String())
}

func TestMuxHandlerMountedMux(t *testing.T) {
	var calls []string
	root, child := NewMux(), NewMux()
	root.Use(tracer("root", &calls))
	child.Use(tracer("mounted", &calls))
	child.Get("/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.PathValue("id"))
	})
	child.NotFound(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "mounted 404")
		w.WriteHeader(http.StatusNotFound)
	})
	root.NotFound(func(http.ResponseWriter, *http.Request) { t.Error("mount must own its fallback") })
	root.Mount("/external", child)
	for _, test := range []struct {
		target string
		calls  []string
	}{
		{"/external/files/42", []string{"mounted", "42"}},
		{"/external/missing", []string{"mounted", "mounted 404"}},
	} {
		t.Run(test.target, func(t *testing.T) {
			calls = nil
			r := httptest.NewRequest("GET", test.target, nil)
			h, pattern := root.Handler(r)
			assert.Equal(t, "/external/", pattern)
			assert.Empty(t, calls)
			assert.Empty(t, r.Pattern)
			h.ServeHTTP(httptest.NewRecorder(), r)
			assert.Equal(t, test.calls, calls)
			assert.Equal(t, test.target, r.URL.Path)
		})
	}
}

func TestMuxHandlerRedirectSnapshot(t *testing.T) {
	for _, code := range []int{0, 301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			m := NewMux(WithCleanPathRedirect(), WithRedirectStatus(code))
			r := httptest.NewRequest("GET", "/a//b?q=%25", nil)
			h, _ := m.Handler(r)
			r.URL.Path, r.URL.RawQuery = "/changed", "changed=1"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if code == 0 {
				code = 307
			}
			assert.Equal(t, code, w.Code)
			assert.Equal(t, "/a/b?q=%25", w.Header().Get("Location"))
		})
	}
}

func TestMuxHandlerConcurrent(t *testing.T) {
	m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching())
	m.Get("/files/{id}", echo("file"))
	m.Get("/docs/", echo("docs"))
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for _, target := range []string{"/files/a%2Fb", "/docs", "/a//b", "/missing"} {
				r := httptest.NewRequest("GET", target, nil)
				h, _ := m.Handler(r)
				h.ServeHTTP(httptest.NewRecorder(), r)
				m.ServeHTTP(httptest.NewRecorder(), r.Clone(r.Context()))
			}
		}()
	}
	workers.Wait()
}

func TestMuxPathPolicyComposition(t *testing.T) {
	tests := []struct {
		name, method, target         string
		status                       int
		endpoint, param              string
		escapedOnly                  bool
		cleanLocation, slashLocation string
		allow                        string
		allowOnSlash                 bool
	}{
		{name: "parameter", method: "GET", target: "/api/files/42?q=%25", status: 200, endpoint: "files", param: "42"},
		{name: "head", method: "HEAD", target: "/api/files/42", status: 200, endpoint: "files", param: "42"},
		{name: "encoded slash in parameter", method: "GET", target: "/api/files/a%2Fb", status: 404, endpoint: "files", param: "a/b", escapedOnly: true},
		{name: "pure slash rejected", method: "GET", target: "/api/files/%2F", status: 404},
		{name: "single decode", method: "GET", target: "/api/files/%252F", status: 200, endpoint: "files", param: "%2F"},
		{name: "encoded dots are data", method: "GET", target: "/api/files/%2e", status: 200, endpoint: "files", param: "."},
		{name: "static escape", method: "GET", target: "/api/%66ixed", status: 200, endpoint: "fixed"},
		{name: "encoded registration", method: "GET", target: "/api/hello", status: 404, endpoint: "hello", escapedOnly: true},
		{name: "exact slash", method: "GET", target: "/api/pinned/", status: 200, endpoint: "pinned"},
		{name: "encoded exact slash", method: "GET", target: "/api/pinned/%2F", status: 404, endpoint: "pinned", escapedOnly: true},
		{name: "unicode", method: "GET", target: "/api/%E4%BD%A0%E5%A5%BD", status: 200, endpoint: "你好"},
		{name: "clean duplicate slash", method: "GET", target: "/api/files//42?q=%25", status: 404, cleanLocation: "/api/files/42?q=%25"},
		{name: "clean dots", method: "GET", target: "/api/a/../files/42?q=1", status: 404, cleanLocation: "/api/files/42?q=1"},
		{name: "clean missing", method: "GET", target: "/api/missing//x?q=1", status: 404, cleanLocation: "/api/missing/x?q=1"},
		{name: "slash redirect", method: "GET", target: "/api/docs?q=1", status: 404, slashLocation: "/api/docs/?q=1"},
		{name: "combined redirect", method: "GET", target: "/api//docs?q=1", status: 404, cleanLocation: "/api/docs?q=1", slashLocation: "/api/docs/?q=1"},
		{name: "method mismatch", method: "POST", target: "/api/files/42", status: 405, allow: "GET, HEAD"},
		{name: "slash method mismatch", method: "POST", target: "/api/docs", status: 404, allow: "GET, HEAD", allowOnSlash: true},
		{name: "missing", method: "GET", target: "/api/missing", status: 404},
		{name: "group root", method: "GET", target: "/api", status: 404},
		{name: "outside", method: "GET", target: "/outside", status: 404},
		{name: "prefix boundary", method: "GET", target: "/apix/files/42", status: 404},
	}
	for mask := 0; mask < 8; mask++ {
		conf := compositionConf(mask)
		t.Run(fmt.Sprintf("policies=%d", mask), func(t *testing.T) {
			for _, mode := range []string{"plain", "with", "route", "mount"} {
				t.Run(mode, func(t *testing.T) {
					var calls []string
					m := NewMuxWithConf(conf)
					m.Use(tracer("root", &calls))
					installCompositionFallbacks(m, "global", &calls)
					var scope Router = m
					prefix := "/api"
					switch mode {
					case "with":
						scope = m.With(tracer("entry", &calls))
					case "route":
						scope = m.With(tracer("entry", &calls)).Route(prefix)
						scope.Use(tracer("group", &calls))
						prefix = ""
					case "mount":
						child := NewMuxWithConf(conf)
						child.Use(tracer("group", &calls))
						installCompositionFallbacks(child, "mounted", &calls)
						m.With(tracer("entry", &calls)).Mount(prefix, child)
						scope, prefix = child, ""
					}
					for _, route := range []struct{ path, endpoint string }{
						{"/files/{id}", "files"}, {"/fixed", "fixed"}, {"/%68ello", "hello"}, {"/你好", "你好"}, {"/docs/", "docs"}, {"/pinned/{$}", "pinned"},
					} {
						scope.With(tracer("leaf", &calls)).Get(prefix+route.path, func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("X-Endpoint", route.endpoint)
							w.Header().Set("X-Pattern", r.Pattern)
							w.Header().Set("X-Param", r.PathValue("id"))
							w.Header().Set("X-Path", r.URL.Path)
							w.Header().Set("X-Raw-Path", r.URL.RawPath)
							w.Header().Set("X-Query", r.URL.RawQuery)
						})
					}
					for _, test := range tests {
						t.Run(test.name, func(t *testing.T) {
							status, location, allow := test.status, "", test.allow
							if test.escapedOnly && conf.EscapedPath {
								status = http.StatusOK
							}
							if test.allowOnSlash {
								if conf.TrailingSlashRedirect {
									status = http.StatusMethodNotAllowed
								} else {
									allow = ""
								}
							}
							parentRedirect := conf.CleanPathRedirect && test.cleanLocation != ""
							if parentRedirect {
								status, location = http.StatusTemporaryRedirect, test.cleanLocation
								if mode != "route" && mode != "mount" && conf.TrailingSlashRedirect && test.slashLocation != "" {
									location = test.slashLocation
								}
							} else if test.cleanLocation == "" && conf.TrailingSlashRedirect && test.slashLocation != "" {
								status, location = http.StatusTemporaryRedirect, test.slashLocation
								if mode == "mount" {
									location = strings.TrimPrefix(location, "/api")
								}
							}
							r := httptest.NewRequest(test.method, test.target, nil)
							urlBefore := *r.URL
							inside := r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/")
							grouped := mode == "route" || mode == "mount"
							wantCalls := []string{"root"}
							if grouped && inside && !parentRedirect {
								wantCalls = append(wantCalls, "entry", "group")
							}
							fallback := ""
							switch status {
							case http.StatusOK:
								if mode == "with" {
									wantCalls = append(wantCalls, "entry")
								}
								wantCalls = append(wantCalls, "leaf")
							case http.StatusNotFound, http.StatusMethodNotAllowed:
								fallback = "global"
								if mode == "mount" && inside {
									fallback = "mounted"
								}
								wantCalls = append(wantCalls, "fallback")
							}
							calls = nil
							w := httptest.NewRecorder()
							m.ServeHTTP(w, r)
							assert.Equal(t, status, w.Code)
							assert.Equal(t, location, w.Header().Get("Location"))
							assert.Equal(t, allow, w.Header().Get("Allow"))
							assert.Equal(t, fallback, w.Header().Get("X-Fallback"))
							assert.Equal(t, wantCalls, calls)
							assert.Equal(t, urlBefore, *r.URL, "routing must preserve the original URL")
							if status == http.StatusOK {
								assert.Equal(t, test.endpoint, w.Header().Get("X-Endpoint"))
								assert.Equal(t, test.param, w.Header().Get("X-Param"))
								wantPath, wantRawPath := r.URL.Path, r.URL.RawPath
								if mode == "mount" {
									wantPath = strings.TrimPrefix(wantPath, "/api")
									wantRawPath = strings.TrimPrefix(wantRawPath, "/api")
								}
								assert.Equal(t, wantPath, w.Header().Get("X-Path"))
								assert.Equal(t, wantRawPath, w.Header().Get("X-Raw-Path"))
								assert.Equal(t, r.URL.RawQuery, w.Header().Get("X-Query"))
								pattern := "GET /api/" + test.endpoint
								switch test.endpoint {
								case "files":
									pattern = "GET /api/files/{id}"
								case "hello":
									pattern = "GET /api/%68ello"
								case "pinned":
									pattern = "GET /api/pinned/{$}"
								}
								if mode == "mount" {
									pattern = strings.Replace(pattern, "/api/", "/", 1)
								}
								assert.Equal(t, pattern, w.Header().Get("X-Pattern"))
							}
							// Query uses the same policies but excludes only the root Use
							// chain; a returned group entry still performs child dispatch.
							calls = calls[:0]
							req := httptest.NewRequest(test.method, test.target, nil)
							h, pattern := m.Handler(req)
							assert.Empty(t, calls)
							assert.Empty(t, req.Pattern)
							assert.Empty(t, req.PathValue("id"))
							if grouped && inside {
								wantPattern := "/api/"
								if req.URL.Path == "/api" {
									wantPattern = "/api"
								}
								assert.Equal(t, wantPattern, pattern)
							}
							queried := httptest.NewRecorder()
							h.ServeHTTP(queried, req)
							assert.Equal(t, status, queried.Code)
							for _, header := range []string{"Location", "Allow", "X-Fallback", "X-Endpoint"} {
								assert.Equal(t, w.Header().Get(header), queried.Header().Get(header), header)
							}
							assert.Equal(t, wantCalls[1:], calls)
						})
					}
				})
			}
		})
	}
}

func TestMuxPathPolicyFallbackScope(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		for _, mount := range []bool{false, true} {
			t.Run(fmt.Sprintf("policies=%d/mount=%t", mask, mount), func(t *testing.T) {
				var calls []string
				root := NewMuxWithConf(compositionConf(mask))
				root.Use(tracer("root", &calls))
				installCompositionFallbacks(root, "global", &calls)
				var child Router
				if mount {
					child = NewMuxWithConf(compositionConf(mask))
					root.With(tracer("entry", &calls)).Mount("/api", child)
				} else {
					child = root.With(tracer("entry", &calls)).Route("/api")
				}
				child.Use(tracer("group", &calls))
				child.Get("/fixed", echo("fixed"))
				for _, stage := range []string{"inherited", "local", "inline reset"} {
					t.Run(stage, func(t *testing.T) {
						switch stage {
						case "local":
							installCompositionFallbacks(child, "local", &calls)
						case "inline reset":
							view := child.With(tracer("unused", &calls))
							view.NotFound(nil)
							view.MethodNotAllowed(nil)
						}
						for _, request := range []requestSpec{{"GET", "/api/missing", ""}, {"POST", "/api/fixed", ""}} {
							calls = nil
							w := httptest.NewRecorder()
							root.ServeHTTP(w, request.request())
							wantCalls := []string{"root", "entry", "group"}
							fallback := "global"
							if mount {
								fallback = ""
							}
							if stage == "local" {
								fallback = "local"
							}
							if fallback != "" {
								wantCalls = append(wantCalls, "fallback")
							}
							assert.Equal(t, fallback, w.Header().Get("X-Fallback"))
							assert.Equal(t, wantCalls, calls)
							status, allow := http.StatusNotFound, ""
							if request.method == "POST" {
								status, allow = http.StatusMethodNotAllowed, "GET, HEAD"
							}
							assert.Equal(t, status, w.Code)
							assert.Equal(t, allow, w.Header().Get("Allow"))
						}
						// Child queries exclude both ancestor entries and child Use;
						// fallback-bound middleware still belongs to the result.
						calls = nil
						target := "/api/missing"
						if mount {
							target = "/missing"
						}
						h, pattern := child.Handler(httptest.NewRequest("GET", target, nil))
						assert.Empty(t, calls)
						assert.Empty(t, pattern)
						h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", target, nil))
						want := []string(nil)
						if stage == "local" || !mount {
							want = []string{"fallback"}
						}
						assert.Equal(t, want, calls)
					})
				}
			})
		}
	}
}

func TestMuxPathPolicyInheritance(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("policies=%d", mask), func(t *testing.T) {
			conf := compositionConf(mask)
			conf.RedirectStatus = http.StatusPermanentRedirect
			root := NewMuxWithConf(conf)
			view := root.With()
			child := view.Route("/api")
			assert.Equal(t, root.conf, view.(*Mux).conf)
			assert.Equal(t, root.conf, child.(*Mux).conf)
			child.Get("/docs/", echo("docs"))
			if conf.TrailingSlashRedirect {
				assert.Equal(t, http.StatusPermanentRedirect, serve(root, "GET", "/api/docs").Code)
			}
			independent := NewMux()
			before := independent.conf
			independent.Get("/docs/", echo("docs"))
			root.Mount("/external", independent)
			assert.Equal(t, before, independent.conf, "Mount must not reconfigure its handler")
			assert.Equal(t, http.StatusNotFound, serve(root, "GET", "/external/docs").Code)
		})
	}
}

func TestMuxEncodedPrefixComposition(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		for _, mount := range []bool{false, true} {
			t.Run(fmt.Sprintf("policies=%d/mount=%t", mask, mount), func(t *testing.T) {
				conf := compositionConf(mask)
				root := NewMuxWithConf(conf)
				var child Router
				if mount {
					child = NewMuxWithConf(conf)
					root.Mount("/api%2Fv1", child)
				} else {
					child = root.Route("/api%2Fv1")
				}
				child.Get("/files/{id}", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Param", r.PathValue("id"))
					w.Header().Set("X-Path", r.URL.Path)
					w.Header().Set("X-Pattern", r.Pattern)
				})
				for _, escapedSlash := range []bool{false, true} {
					target := "/api%252Fv1/files/42"
					if escapedSlash {
						target = "/api%2Fv1/files/42"
					}
					r := httptest.NewRequest("GET", target, nil)
					before := *r.URL
					w := httptest.NewRecorder()
					root.ServeHTTP(w, r)
					if conf.EscapedPath != escapedSlash {
						assert.Equal(t, http.StatusNotFound, w.Code)
						continue
					}
					assert.Equal(t, http.StatusOK, w.Code)
					assert.Equal(t, "42", w.Header().Get("X-Param"))
					path, pattern := r.URL.Path, "GET /api%2Fv1/files/{id}"
					if mount {
						path, pattern = "/files/42", "GET /files/{id}"
					}
					assert.Equal(t, path, w.Header().Get("X-Path"))
					assert.Equal(t, pattern, w.Header().Get("X-Pattern"))
					assert.Equal(t, before, *r.URL)
				}
			})
		}
	}
}

func TestMuxPathPolicyBoundaries(t *testing.T) {
	t.Run("ordinary percent parameter", func(t *testing.T) {
		m := NewMux()
		m.Get("/files/{id}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.PathValue("id")))
		})
		w := serve(m, http.MethodGet, "/files/%252F")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "%2F", w.Body.String())
	})
	t.Run("clean without escaped matching", func(t *testing.T) {
		m := NewMux(WithCleanPathRedirect())
		m.Get("/", echo("fallback"))
		w := serve(m, http.MethodGet, "/a%2Fb//c?q=%25")
		assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
		assert.Equal(t, "/a%2Fb/c?q=%25", w.Header().Get("Location"))
	})
	t.Run("disabled cleaning", func(t *testing.T) {
		m := NewMux(WithEscapedPathMatching())
		m.Get("/", echo("fallback"))
		w := serve(m, http.MethodGet, "/a%2Fb//c")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "fallback", w.Body.String())
		assert.Empty(t, w.Header().Get("Location"))
	})
	t.Run("disabled cleaning keeps slash target", func(t *testing.T) {
		m := NewMux(WithEscapedPathMatching(), WithTrailingSlashRedirect())
		m.Get("/a%2Fb//docs/", echo("docs"))
		w := serve(m, http.MethodGet, "/a%2Fb//docs?q=%25")
		assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
		assert.Equal(t, "/a%2Fb//docs/?q=%25", w.Header().Get("Location"))
	})
	t.Run("custom redirect status", func(t *testing.T) {
		m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching(), WithRedirectStatus(http.StatusPermanentRedirect))
		m.Get("/", echo("fallback"))
		w := serve(m, http.MethodPost, "/a%2Fb//c?q=%25")
		assert.Equal(t, http.StatusPermanentRedirect, w.Code)
		assert.Equal(t, "/a%2Fb/c?q=%25", w.Header().Get("Location"))
	})
}

func TestMuxEscapedSlashCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		targets  []string
	}{
		{"parameter", []string{"GET /files/{id}"}, []string{"/files/%2F", "/files/%2f", "/files/a%2Fb", "/files/%252F", "/files/%2F%2F", "/files/42", "/files/"}},
		{"static", []string{"GET /files/{id}", "GET /files/%2F"}, []string{"/files/%2F", "/files/%2f", "/files/%252F"}},
		{"catch-all fallback", []string{"GET /files/{id}", "GET /files/{rest...}"}, []string{"/files/%2F", "/files/%2f", "/files/a%2Fb", "/files/%252F", "/files/a/b"}},
		{"nested fallback", []string{"GET /files/{id}/info", "GET /files/{rest...}"}, []string{"/files/%2F/info", "/files/a%2Fb/info", "/files/%252F/info"}},
		{"method candidates", []string{"GET /files/{id}", "POST /files/{rest...}"}, []string{"/files/%2F", "/files/42"}},
		{"root fallback", []string{"GET /files/{id}", "/"}, []string{"/files/%2F", "/files/%252F"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching(), WithTrailingSlashRedirect())
			standard := http.NewServeMux()
			registerRoutingComparison(m, standard, test.patterns)
			for _, target := range test.targets {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
					t.Run(method+" "+target, func(t *testing.T) {
						want := observeRouting(standard, method, target, "")
						assert.Equal(t, want, observeRouting(m, method, target, ""))
					})
				}
			}
		})
	}
}

func TestMuxCleanRedirectCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		targets  []string
	}{
		{"encoded slash", []string{"GET /a%2Fb/c"}, []string{"/a%2Fb//c", "/a%2fb//c?q=a%2Fb&x=%25", "/a%252Fb//c"}},
		{"encoded dots", []string{"GET /files/{rest...}"}, []string{"/files/%2e//a", "/files/%2E%2e//a", "/files/%252e//a"}},
		{"unicode", []string{"GET /你好/c"}, []string{"/你好//c?q=海诺&x=%25", "/%E4%BD%A0%E5%A5%BD//c"}},
		{"slash probe", []string{"GET /a%2Fb/docs/"}, []string{"/a%2Fb/docs", "/a%2Fb//docs?q=1", "/a%2Fb/./docs"}},
		{"explicit root", []string{"GET /a%2Fb/docs", "GET /a%2Fb/docs/"}, []string{"/a%2Fb/docs", "/a%2Fb//docs"}},
		{"unmatched", nil, []string{"/a%2Fb//missing?q=%25", "/a%252Fb//missing"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewMux(WithCleanPathRedirect(), WithEscapedPathMatching(), WithTrailingSlashRedirect())
			standard := http.NewServeMux()
			registerRoutingComparison(m, standard, test.patterns)
			for _, target := range test.targets {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
					t.Run(method+" "+target, func(t *testing.T) {
						want := observeRouting(standard, method, target, "")
						assert.Equal(t, want, observeRouting(m, method, target, ""))
					})
				}
			}
		})
	}
}

func TestMuxConnectHostPriority(t *testing.T) {
	tests := []struct {
		name         string
		patterns     []string
		target, host string
	}{
		{"global exact", []string{"CONNECT /docs"}, "http://probe.example/docs", "dispatch.example"},
		{"same host parameter", []string{"CONNECT probe.example/docs/{id}"}, "http://probe.example/docs/42", "probe.example"},
		{"probe redirect before final exact", []string{"CONNECT probe.example/docs/", "CONNECT dispatch.example/docs"}, "http://probe.example/docs", "dispatch.example"},
		{"probe exact prevents final host redirect", []string{"CONNECT probe.example/docs", "CONNECT dispatch.example/docs/"}, "http://probe.example/docs", "dispatch.example"},
		{"probe methods", []string{"GET probe.example/docs", "POST probe.example/docs", "CONNECT dispatch.example/docs/"}, "http://probe.example/docs", "dispatch.example"},
		{"final host methods excluded", []string{"POST dispatch.example/docs", "CONNECT elsewhere.example/unused"}, "http://probe.example/docs", "dispatch.example"},
		{"final host parameter", []string{"CONNECT probe.example/docs/{name}", "CONNECT dispatch.example/docs/{id}"}, "http://probe.example/docs/42", "dispatch.example"},
		{"probe port", []string{"CONNECT probe.example:443/docs/", "CONNECT dispatch.example/docs"}, "http://probe.example:443/docs", "dispatch.example"},
		{"global fallback", []string{"CONNECT other.example/unused", "CONNECT /docs/{id}"}, "http://probe.example/docs/42", "dispatch.example"},
		{"unclean slash target", []string{"CONNECT probe.example/a/../docs/"}, "http://probe.example/a/../docs?q=%25", "dispatch.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for mask := 0; mask < 4; mask++ {
				clean, escaped := mask&1 != 0, mask&2 != 0
				t.Run(fmt.Sprintf("clean=%t/escaped=%t", clean, escaped), func(t *testing.T) {
					m := NewMuxWithConf(MuxConf{CleanPathRedirect: clean, EscapedPath: escaped, TrailingSlashRedirect: true})
					standard := http.NewServeMux()
					registerRoutingComparison(m, standard, test.patterns)
					want := observeRouting(standard, http.MethodConnect, test.target, test.host)
					assert.Equal(t, want, observeRouting(m, http.MethodConnect, test.target, test.host))
				})
			}
		})
	}
}

func TestMuxConnectRedirectRequiresPolicy(t *testing.T) {
	// CONNECT matching dispatches on the Host header; build requests like
	// observeRouting does — as GET with the method swapped afterwards.
	connect := func(target string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Method = http.MethodConnect
		return r
	}

	// Without the trailing-slash policy a CONNECT request never probes for one.
	off := NewMuxWithConf(MuxConf{})
	off.Handle("CONNECT example.com/db", echo("db"))
	assert.Equal(t, "db", recorder(off, connect("http://example.com/db")).Body.String())

	// With the policy on, an already slash-terminated path skips the probe too.
	on := NewMuxWithConf(MuxConf{TrailingSlashRedirect: true})
	on.Handle("CONNECT example.com/db/", echo("db"))
	assert.Equal(t, "db", recorder(on, connect("http://example.com/db/")).Body.String())
}

// ServeMux alone decides which generated tables are legal; Mino must
// accept every one it accepts.
func TestMuxLegalRouteTables(t *testing.T) {
	patterns := []string{
		"/", "/{$}", "/a", "/a/", "/a/{$}", "/a/{id}", "/a/{rest...}",
		"/{name}/b", "/a/{id}/b", "/a/fixed", "GET /", "GET /a", "HEAD /a",
		"POST /a", "GET /a/{id}", "POST /a/{rest...}", "GET /a/fixed", "CUSTOM /a",
		"example.com/a", "GET example.com/a/{id}", "GET example.com/a/fixed",
		"POST other.example/a", "GET /%61", "GET /中文", "GET /a%2Fb",
		"GET /a/%2F", "GET /a/%25", "GET /a/%7B", "GET /a/%2e",
		"GET /a/}", "GET /a/%7D", "GET /a/%7Bid%7D", "GET /a/{名字}",
	}
	var requests []requestSpec
	for _, target := range []string{
		"/", "/a", "/a/", "/a/x", "/a/fixed", "/a/x/b", "/a/fixed/b",
		"/other/b", "/a/b/", "/missing", "/a/x/y", "/%61", "/中文",
		"/%E4%B8%AD%E6%96%87", "/a%2Fb", "/a%2fb", "/a/%2F", "/a/%2f",
		"/a/a%2Fb", "/a/%252F", "/a/%2F%2F", "/a/%25", "/a/%7B",
		"/a/%7D", "/a/%7Bid%7D", "/a/%2F/", "/a/%2F/more", "/a/%2e", "/a/%252e", "/a//x?q=%25",
		"/a/./x", "/a/z/../x", "/a/x?q=中文&x=%25",
	} {
		for _, method := range []string{"GET", "HEAD", "POST", "CUSTOM"} {
			requests = append(requests, requestSpec{method, target, ""})
		}
	}
	for _, host := range []string{"example.com", "example.com:8080", "other.example", "EXAMPLE.com"} {
		for _, target := range []string{"/a", "/a/x", "/a/fixed", "/missing"} {
			for _, method := range []string{"GET", "HEAD", "POST", "DELETE"} {
				requests = append(requests, requestSpec{method, target, host})
			}
		}
	}
	accepted, rejected, registrations := 0, 0, 0
	check := func(name string, table []string) {
		if !standardRouteTable(table) {
			rejected++
			t.Logf("ServeMux rejected %s: %q", name, table)
			return
		}
		accepted++
		registrations += len(routeTableOrders(table))
		t.Run(name, func(t *testing.T) {
			checkLegalRouteTable(t, table, requests)
		})
	}
	check("empty", nil)
	for i, first := range patterns {
		if !assert.True(t, standardRouteTable([]string{first}), "corpus pattern must be legal: %q", first) {
			return
		}
		check(fmt.Sprintf("single-%02d", i), []string{first})
		for j := i + 1; j < len(patterns); j++ {
			check(fmt.Sprintf("pair-%02d-%02d", i, j), []string{first, patterns[j]})
		}
	}
	// All triples of the first ten path shapes exercise competing static,
	// parameter, subtree and exact-slash candidates in a single table.
	for i := range 10 {
		for j := i + 1; j < 10; j++ {
			for k := j + 1; k < 10; k++ {
				check(fmt.Sprintf("triple-%02d-%02d-%02d", i, j, k), []string{patterns[i], patterns[j], patterns[k]})
			}
		}
	}
	assert.Greater(t, accepted, 400)
	assert.Greater(t, rejected, 0)
	t.Logf("generated tables: accepted=%d rejected-by-ServeMux=%d; registration permutations=%d; request cases/table=%d", accepted, rejected, registrations, len(requests))
}

func TestMuxLegalRouteTableScenarios(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		requests []requestSpec
	}{
		{"method hierarchy", []string{"/a/{rest...}", "GET /a/{id}", "HEAD /a/fixed", "GET /a/fixed", "POST /a/{rest...}"}, []requestSpec{
			{"GET", "/a/fixed", ""}, {"HEAD", "/a/fixed", ""}, {"HEAD", "/a/42", ""}, {"POST", "/a/42/extra", ""}, {"DELETE", "/a/42", ""},
		}},
		{"host specificity exception", []string{"GET /a/fixed", "example.com/a/{rest...}", "GET example.com/a/{id}", "HEAD example.com/a/{id}", "GET /a/{id}"}, []requestSpec{
			{"GET", "/a/fixed", "example.com:8080"}, {"HEAD", "/a/fixed", "example.com"}, {"POST", "/a/fixed", "example.com"}, {"GET", "/a/fixed", "other.example"}, {"GET", "/a/42", "example.com"},
		}},
		{"backtracking", []string{"/a/{rest...}", "/a/fixed/{id}/end", "/a/{name}/other", "/a/fixed/literal/end"}, []requestSpec{
			{"GET", "/a/fixed/42/end", ""}, {"GET", "/a/fixed/literal/end", ""}, {"GET", "/a/fixed/other", ""}, {"GET", "/a/fixed/42/missing", ""},
		}},
		{"exact and subtree slash", []string{"GET /{$}", "GET /docs/", "GET /docs/{$}", "POST /docs", "GET /docs/{id}"}, []requestSpec{
			{"GET", "/", ""}, {"GET", "/docs", ""}, {"POST", "/docs", ""}, {"DELETE", "/docs", ""}, {"GET", "/docs/", ""}, {"GET", "/docs/42", ""},
		}},
		{"connect authority", []string{"CONNECT probe.example/docs", "CONNECT dispatch.example/docs/", "GET probe.example/docs", "POST dispatch.example/docs"}, []requestSpec{
			{"CONNECT", "http://probe.example/docs", "dispatch.example"}, {"GET", "http://probe.example/docs", "dispatch.example"}, {"PUT", "http://probe.example/docs", "dispatch.example"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !assert.True(t, standardRouteTable(test.patterns), "fixture must be accepted by ServeMux") {
				return
			}
			checkLegalRouteTable(t, test.patterns, test.requests)
		})
	}
}

func BenchmarkMuxHandler(b *testing.B) {
	tests := []struct {
		name, method, target string
		clean, escaped       bool
	}{
		{"static", "GET", "/hello", false, false},
		{"parameter", "GET", "/users/42", false, false},
		{"static-options", "GET", "/hello", true, true},
		{"encoded-parameter", "GET", "/users/a%2Fb", true, true},
		{"slash-redirect", "GET", "/docs?q=1", false, false},
		{"clean-redirect", "GET", "/a//b?q=1", true, true},
		{"not-found", "GET", "/missing", false, false},
		{"method-not-allowed", "POST", "/hello", false, false},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			m := NewMuxWithConf(MuxConf{CleanPathRedirect: test.clean, EscapedPath: test.escaped, TrailingSlashRedirect: true})
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			m.Handle("GET /hello", handler)
			m.Handle("GET /users/{id}", handler)
			m.Handle("GET /docs/", handler)
			r := httptest.NewRequest(test.method, test.target, nil)
			var h http.Handler
			var pattern string
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h, pattern = m.Handler(r)
			}
			runtime.KeepAlive(h)
			runtime.KeepAlive(pattern)
		})
	}
}

func BenchmarkMuxCompatibility(b *testing.B) {
	tests := []struct {
		name                 string
		patterns             []string
		method, target, host string
		clean, escaped       bool
	}{
		{name: "static", patterns: []string{"GET /hello"}, method: "GET", target: "/hello"},
		{name: "parameter", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/42"},
		{name: "parameter-three-bytes", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/123"},
		{name: "static-options", patterns: []string{"GET /hello"}, method: "GET", target: "/hello", clean: true, escaped: true},
		{name: "parameter-options", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/42", clean: true, escaped: true},
		{name: "encoded-parameter", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/a%2Fb", clean: true, escaped: true},
		{name: "encoded-slash", patterns: []string{"GET /users/{id}"}, method: "GET", target: "/users/%2F", clean: true, escaped: true},
		{name: "catch-all-fallback", patterns: []string{"GET /users/{id}", "GET /users/{rest...}"}, method: "GET", target: "/users/%2F", clean: true, escaped: true},
		{name: "clean-redirect", patterns: []string{"GET /a%2Fb/c"}, method: "GET", target: "/a%2Fb//c?q=1", clean: true, escaped: true},
		{name: "not-found", patterns: []string{"GET /hello"}, method: "GET", target: "/missing"},
		{name: "method-not-allowed", patterns: []string{"GET /hello"}, method: "POST", target: "/hello"},
		{name: "connect-match", patterns: []string{"CONNECT /docs"}, method: "CONNECT", target: "http://probe.example/docs", host: "dispatch.example"},
		{name: "connect-host-miss", patterns: []string{"CONNECT probe.example/docs", "CONNECT dispatch.example/docs/"}, method: "CONNECT", target: "http://probe.example/docs", host: "dispatch.example"},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			m := NewMuxWithConf(MuxConf{CleanPathRedirect: test.clean, EscapedPath: test.escaped, TrailingSlashRedirect: true})
			var value string
			for _, pattern := range test.patterns {
				m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { value = r.PathValue("id") })
			}
			template := *httptest.NewRequest(http.MethodGet, test.target, nil)
			template.Method = test.method
			if test.host != "" {
				template.Host = test.host
			}
			r := new(http.Request)
			w := &discardWriter{header: make(http.Header)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Count fresh request metadata, including initial PathValue storage.
				*r = template
				clear(w.header)
				m.ServeHTTP(w, r)
			}
			runtime.KeepAlive(value)
		})
	}
}
