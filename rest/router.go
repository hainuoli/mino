package rest

import (
	"net/http"
	"strings"

	"github.com/hainuoli/mino/rest/chain"
)

type (
	// Route is an entry yielded by Routes and Walk.
	Route struct {
		// Method is the HTTP method of the route.
		Method string
		// Host is the host pattern the route is bound to, if any.
		Host string
		// Pattern is relative to the mux returned by Routes; Walk
		// reports the complete leaf pattern.
		Pattern string
		// Handler is the final handler of a leaf route.
		Handler http.Handler
		// Middlewares lists wrappers attached to this entry, outermost first.
		Middlewares []chain.Middleware
		// SubRoutes holds the mounted subtree, if any.
		SubRoutes Routes
		// Mount reports a mount point, including entries created by Route.
		Mount bool
	}

	// Routes exposes the registered routes and the middleware stack in effect.
	Routes interface {
		// Routes returns a snapshot of all registered routes:
		//
		//	for _, r := range rt.Routes() {
		//		fmt.Println(r.Method, r.Pattern)
		//	}
		//
		// Patterns are relative to this mux's base path; use Walk for
		// complete leaf paths.
		Routes() []Route

		// Middlewares returns a copy of the local middleware stack,
		// outermost first:
		//
		//	rt.Use(logger, recoverer)
		//	rt.Middlewares() // logger, recoverer
		Middlewares() []chain.Middleware
	}

	// Router is the public routing API for registering routes,
	// middlewares, grouping and mounting external handlers.
	Router interface {
		http.Handler
		Routes

		// Handler looks up the handler and pattern selected for r without
		// running middleware or modifying r. It always returns a non-nil
		// handler: a registered entry, a redirect or a fallback. Pattern
		// and PathValue are not populated; use ServeHTTP for serving.
		Handler(r *http.Request) (http.Handler, string)

		// Use appends middlewares to this router's stack. They wrap dispatch,
		// including 404 and 405 responses, even without routes:
		//
		//	r.Use(logger, recoverer)
		//	r.Get("/ping", h)
		//
		// requests to /ping flow logger -> recoverer -> h.
		Use(...chain.Middleware)

		// With returns an inline registration view sharing the owner's routes.
		// Its middleware wraps handlers registered through the view only:
		//
		//	api := r.With(auth)
		//	api.Get("/profile", h) // requests flow auth -> h
		//	r.Get("/login", h2)    // auth does not apply here
		//
		// Nested With calls accumulate inline middleware. For an entire
		// HTTP-entry wrapper, use chain.New(mws...).Then(r).
		With(...chain.Middleware) Router

		// Route creates a subrouter grouping all routes under prefix:
		//
		//	api := r.Route("/api")
		//	api.Get("/ping", h)
		//
		// serves GET /api/ping. The prefix must be literal.
		Route(prefix string) Router

		// Mount attaches an external http.Handler, such as another router,
		// under prefix:
		//
		//	r.Mount("/gin", ginEngine)
		//
		// requests under /gin are dispatched to the handler with
		// the prefix stripped. The prefix must be literal.
		Mount(prefix string, handler http.Handler)

		// Handle registers handler for pattern, which may carry its own method:
		//
		//	r.Handle("/ping", h)     // any method
		//	r.Handle("GET /ping", h) // GET only
		//
		// A pattern without a method is an any-method fallback; HEAD falls
		// back to GET. Parameters occupy whole segments; {name...} is last
		// only. A trailing slash matches a subtree; /path/{$} pins an exact
		// slash. Invalid patterns and duplicate endpoints panic during
		// registration.
		Handle(pattern string, handler http.Handler)

		// HandleFunc is Handle taking a function with the standard net/http signature:
		//
		//	r.HandleFunc("GET /hi", hi)
		//
		// is equivalent to:
		//
		//	r.Handle("GET /hi", http.HandlerFunc(hi))
		HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Get registers fn for GET requests matching pattern:
		//
		//	r.Get("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("GET /ping", h)
		//
		// HEAD falls back to matching GET routes when no HEAD route matches.
		Get(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Head registers fn for HEAD requests matching pattern:
		//
		//	r.Head("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("HEAD /ping", h)
		Head(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Post registers fn for POST requests matching pattern:
		//
		//	r.Post("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("POST /ping", h)
		Post(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Put registers fn for PUT requests matching pattern:
		//
		//	r.Put("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("PUT /ping", h)
		Put(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Patch registers fn for PATCH requests matching pattern:
		//
		//	r.Patch("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("PATCH /ping", h)
		Patch(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Delete registers fn for DELETE requests matching pattern:
		//
		//	r.Delete("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("DELETE /ping", h)
		Delete(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Connect registers fn for CONNECT requests matching pattern:
		//
		//	r.Connect("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("CONNECT /ping", h)
		Connect(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Options registers fn for OPTIONS requests matching pattern:
		//
		//	r.Options("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("OPTIONS /ping", h)
		Options(pattern string, handler func(http.ResponseWriter, *http.Request))

		// Trace registers fn for TRACE requests matching pattern:
		//
		//	r.Trace("/ping", h)
		//
		// is equivalent to:
		//
		//	r.Handle("TRACE /ping", h)
		Trace(pattern string, handler func(http.ResponseWriter, *http.Request))

		// NotFound registers fn as the fallback for requests that match no route:
		//
		//	r.Get("/ping", h)
		//	// GET /missing falls back to fn.
		//
		// A path that matches but a method that does not is MethodNotAllowed,
		// not NotFound. Passing nil restores the default.
		NotFound(handler http.HandlerFunc)

		// MethodNotAllowed registers fn for requests whose path matches a
		// route but whose method does not:
		//
		//	r.Get("/ping", h)
		//	// POST /ping falls back to fn, not NotFound.
		//
		// Passing nil restores the default 405 response.
		MethodNotAllowed(handler http.HandlerFunc)
	}

	// WalkFunc is invoked once per leaf route during a Walk;
	// returning an error aborts the walk.
	WalkFunc func(Route) error
)

// Walk traverses the routes tree depth-first, joining parent prefixes,
// hosts and middlewares onto each route, and invokes fn once per leaf
// route. Mounted subtrees are recursed into, never yielded themselves.
func Walk(routes Routes, fn WalkFunc) error {
	if routes == nil {
		return nil
	}
	return walk(routes, fn, "", "", nil)
}

func joinMiddleware(a, b []chain.Middleware) []chain.Middleware {
	mws := make([]chain.Middleware, 0, len(a)+len(b))
	mws = append(mws, a...)
	mws = append(mws, b...)
	return mws
}

// joinPattern joins a and b with exactly one separating slash,
// treating "" and "/" as the root.
func joinPattern(a, b string) string {
	if a == "" || a == "/" {
		if b == "" {
			return a
		}
		return b
	}
	if b == "" || b == "/" {
		return a
	}
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

// walk recurses through subroutes, accumulating prefix, host and
// middlewares along the way.
func walk(routes Routes, fn WalkFunc, prefix, host string, parentMw []chain.Middleware) error {
	mws := joinMiddleware(parentMw, routes.Middlewares())
	for _, route := range routes.Routes() {
		route.Pattern = joinPattern(prefix, route.Pattern)
		if route.Host == "" {
			route.Host = host
		}
		route.Middlewares = joinMiddleware(mws, route.Middlewares)

		if route.SubRoutes != nil {
			if err := walk(route.SubRoutes, fn, route.Pattern, route.Host, route.Middlewares); err != nil {
				return err
			}
			continue
		}

		if fn != nil {
			if err := fn(route); err != nil {
				return err
			}
		}
	}
	return nil
}
