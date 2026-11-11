package rest

import (
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hainuoli/mino/rest/chain"
)

const (
	allMethods           = ""
	allowHeader          = "Allow"
	allowMethodSeparator = ", "
)

var _ Router = &Mux{}

var (
	errInvalidHost           = errors.New("invalid host")
	errInvalidMethod         = errors.New("invalid HTTP method")
	errInvalidPrefix         = errors.New("invalid prefix")
	errInvalidRedirectStatus = errors.New("invalid redirect status")
	errNilHandler            = errors.New("nil handler")
	errNilMountHandler       = errors.New("nil mount handler")
)

type (
	// A Mux is the default Router implementation.
	// Finish configuration before serving; registration and middleware
	// changes must not run concurrently with requests.
	Mux struct {
		trees methodTrees
		hosts map[string]methodTrees

		// mws is the local stack built by Use.
		mws []chain.Middleware
		// handler caches the middleware chain around routeHTTP. A nil
		// handler means dispatch needs no middleware wrapper.
		handler    http.Handler
		notFound   http.HandlerFunc
		notAllowed http.HandlerFunc

		parent   *Mux
		entry    *ChainHandler
		basePath string

		// inline marks routers derived by With: they defer
		// registration and serving to their parent.
		inline bool
		conf   MuxConf
	}

	// MuxConf selects path matching and automatic redirect policies.
	// The zero value disables every policy.
	MuxConf struct {
		// CleanPathRedirect normalizes literal slash and dot segments before
		// dispatch, redirecting even when the cleaned path has no route.
		CleanPathRedirect bool

		// EscapedPath matches patterns and requests segment by segment after
		// one unescape, preserving encoded slashes inside their original segment.
		EscapedPath bool

		// TrailingSlashRedirect redirects /tree to /tree/ when
		// only the slash form is registered.
		TrailingSlashRedirect bool

		// RedirectStatus is the status of automatic path redirects. Zero means 307.
		RedirectStatus int
	}

	// Option customizes a Mux during NewMux.
	Option func(*Mux)
)

// NewMux returns a Mux with all policies disabled and opts applied:
//
//	mux := rest.NewMux()
//	mux.Get("/ping", ping)
//
// A Mux works directly as an http.Handler.
func NewMux(opts ...Option) *Mux {
	m := NewMuxWithConf(MuxConf{})
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// NewMuxWithConf creates a Mux with exactly the supplied configuration.
// Zero-value fields keep every policy off; RedirectStatus zero selects 307.
func NewMuxWithConf(conf MuxConf) *Mux {
	conf.RedirectStatus = redirectStatus(conf.RedirectStatus)
	return &Mux{conf: conf}
}

// WithCleanPathRedirect enables path normalization before dispatch.
// The default is off; redirects preserve encoded segment boundaries and the query.
func WithCleanPathRedirect() Option {
	return func(m *Mux) {
		m.conf.CleanPathRedirect = true
	}
}

// WithRedirectStatus sets the status of automatic path redirects. Supported codes
// are 301, 302, 303, 307 and 308; zero selects the default 307.
func WithRedirectStatus(code int) Option {
	return func(m *Mux) {
		m.conf.RedirectStatus = redirectStatus(code)
	}
}

// WithEscapedPathMatching enables segment-wise unescaping for patterns and
// requests; encoded slashes stay in their segment. Configure before registering.
func WithEscapedPathMatching() Option {
	return func(m *Mux) {
		m.conf.EscapedPath = true
	}
}

// WithTrailingSlashRedirect enables redirects from a subtree root to its slash form.
func WithTrailingSlashRedirect() Option {
	return func(m *Mux) {
		m.conf.TrailingSlashRedirect = true
	}
}

// ServeHTTP dispatches the request through the compiled middleware chain
// into the routing tree. An unmatched request receives MethodNotAllowed
// (with Allow) or NotFound; empty muxes also run their middleware first:
//
//	http.ListenAndServe(":8080", mux)
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.inline {
		m.parent.ServeHTTP(w, r)
		return
	}

	if m.handler == nil {
		m.routeHTTP(w, r)
		return
	}
	m.handler.ServeHTTP(w, r)
}

// Handler looks up the handler and pattern selected for r without
// running middleware or modifying r. It always returns a non-nil
// handler: a registered entry, a redirect or a fallback. Pattern and
// PathValue are not populated; use ServeHTTP for serving.
func (m *Mux) Handler(r *http.Request) (http.Handler, string) {
	if m.inline {
		return m.parent.Handler(r)
	}

	requestPath, targetPath, clean := r.URL.Path, "", false
	if m.conf.EscapedPath || m.conf.CleanPathRedirect {
		requestPath, targetPath, clean = m.pathView(r)
	}
	if requestPath == "" && r.Method != http.MethodConnect {
		requestPath = "/"
	}
	host := ""
	if m.hosts != nil {
		host = requestHost(r)
	}
	if r.Method == http.MethodConnect {
		if target := m.connectRedirectPath(r, requestPath); target != "" {
			return m.redirectHandler(redirectLocation(url.URL{Path: target}, r.URL.RawQuery)), target
		}
	}

	_, matched, exact := m.match(host, r.Method, requestPath, nil)
	slashPath := ""
	if clean || (!exact && (matched == nil || !matched.subroute)) {
		var pattern, location string
		slashPath, pattern, location = m.redirectTarget(r, host, requestPath, targetPath, matched, clean, exact)
		if location != "" {
			return m.redirectHandler(location), pattern
		}
	}
	if matched != nil {
		return matched.handler, matched.pattern
	}
	if m.conf.TrailingSlashRedirect && slashPath == "" && !strings.HasSuffix(requestPath, "/") {
		slashPath = requestPath + "/"
	}
	if allowed := m.matchingMethods(r, host, requestPath, slashPath); len(allowed) != 0 {
		allow := strings.Join(allowed, allowMethodSeparator)
		fallback := m.methodNotAllowedHandler()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(allowHeader, allow)
			fallback.ServeHTTP(w, r)
		}), ""
	}
	return m.notFoundHandler(), ""
}

// Routes returns a snapshot of all registered routes:
//
//	for _, r := range rt.Routes() {
//		fmt.Println(r.Method, r.Pattern)
//	}
//
// Patterns are relative to this mux's base path; use Walk for
// complete leaf paths.
func (m *Mux) Routes() []Route {
	var routes []Route
	for method, n := range m.trees {
		routes = append(routes, n.routes("", method, m.basePath)...)
	}
	for host, trees := range m.hosts {
		for method, n := range trees {
			routes = append(routes, n.routes(host, method, m.basePath)...)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Pattern < b.Pattern
	})
	return routes
}

// Middlewares returns a copy of the local middleware stack, outermost first:
//
//	rt.Use(logger, recoverer)
//	rt.Middlewares() // logger, recoverer
func (m *Mux) Middlewares() []chain.Middleware {
	return append([]chain.Middleware(nil), m.mws...)
}

// Use appends middlewares to this router's stack. They wrap dispatch,
// including 404 and 405 responses, even without routes:
//
//	r.Use(logger, recoverer)
//	r.Get("/ping", h)
//
// requests to /ping flow logger -> recoverer -> h.
func (m *Mux) Use(mws ...chain.Middleware) {
	// Rebuilding an unchanged chain would re-run every middleware
	// constructor, so a no-op call must leave the compiled handler alone.
	if len(mws) == 0 {
		return
	}
	for i, mw := range mws {
		if mw == nil {
			panic(fmt.Errorf("mino: Use middleware[%d] is nil", i))
		}
	}

	m.mws = append(m.mws, mws...)
	if !m.inline {
		m.handler = chain.New(m.mws...).Then(http.HandlerFunc(m.routeHTTP))
	}
}

// With returns an inline registration view sharing the owner's routes.
// Its middleware wraps handlers registered through the view only:
//
//	api := r.With(auth)
//	api.Get("/profile", h) // requests flow auth -> h
//	r.Get("/login", h2)    // auth does not apply here
//
// Nested With calls accumulate inline middleware. For an entire
// HTTP-entry wrapper, use chain.New(mws...).Then(r).
func (m *Mux) With(mws ...chain.Middleware) Router {
	for i, mw := range mws {
		if mw == nil {
			panic(fmt.Errorf("mino: With middleware[%d] is nil", i))
		}
	}
	parent := m
	var inherited []chain.Middleware
	if m.inline {
		parent = m.parent
		inherited = m.mws
	}

	return &Mux{
		mws:      joinMiddleware(inherited, mws),
		parent:   parent,
		basePath: m.basePath,
		inline:   true,
		conf:     m.conf,
	}
}

// Route creates a subrouter grouping all routes under prefix:
//
//	api := r.Route("/api")
//	api.Get("/ping", h)
//
// serves GET /api/ping. The prefix must be literal.
func (m *Mux) Route(prefix string) Router {
	prefix = normalizePrefix(prefix)
	child := NewMuxWithConf(m.conf)

	child.basePath = m.basePath
	if prefix != "/" {
		child.basePath = m.withBase(prefix)
	}

	child.parent = m
	if m.inline {
		child.parent = m.parent
	}

	child.entry = &ChainHandler{handler: child, chain: child}
	m.attach(prefix, child.entry)
	return child
}

// Mount attaches an external http.Handler, such as another router,
// under prefix:
//
//	r.Mount("/gin", ginEngine)
//
// requests under /gin are dispatched to the handler with
// the prefix stripped. The prefix must be literal.
func (m *Mux) Mount(prefix string, handler http.Handler) {
	if handler == nil {
		panic(fmt.Errorf("mino: %w for prefix %q", errNilMountHandler, prefix))
	}
	if h, ok := handler.(http.HandlerFunc); ok && h == nil {
		panic(fmt.Errorf("mino: %w for prefix %q", errNilMountHandler, prefix))
	}

	prefix = normalizePrefix(prefix)

	if mountPrefix := m.withBase(prefix); mountPrefix != "/" {
		if m.conf.EscapedPath {
			mountPrefix = unescapePath(mountPrefix)
		}
		handler = stripPrefix(mountPrefix, handler)
	}

	m.attach(prefix, &ChainHandler{handler: handler, chain: handler})
}

// Handle registers handler for pattern, which may carry its own method:
//
//	r.Handle("/ping", h)     // any method
//	r.Handle("GET /ping", h) // GET only
//
// A pattern without a method is an any-method fallback; HEAD falls back
// to GET. Parameters occupy whole segments; {name...} is last only. A
// trailing slash matches a subtree; /path/{$} pins an exact slash.
// Invalid patterns and duplicate endpoints panic during registration.
func (m *Mux) Handle(pattern string, handler http.Handler) {
	original := pattern
	method := allMethods
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		method = pattern[:i]
		if method != "" && !validMethod(method) {
			panic(fmt.Errorf("mino: %w %q in pattern %q", errInvalidMethod, method, pattern))
		}
		pattern = strings.TrimLeft(pattern[i+1:], " \t")
	}
	m.handle(method, pattern, original, handler)
}

// HandleFunc is Handle taking a function with the standard net/http signature:
//
//	r.HandleFunc("GET /hi", hi)
//
// is equivalent to:
//
//	r.Handle("GET /hi", http.HandlerFunc(hi))
func (m *Mux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

// Get registers fn for GET requests matching pattern:
//
//	r.Get("/ping", h)
//
// is equivalent to:
//
//	r.Handle("GET /ping", h)
//
// HEAD falls back to matching GET routes when no HEAD route matches.
func (m *Mux) Get(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodGet+" "+pattern, http.HandlerFunc(handler))
}

// Head registers fn for HEAD requests matching pattern:
//
//	r.Head("/ping", h)
//
// is equivalent to:
//
//	r.Handle("HEAD /ping", h)
func (m *Mux) Head(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodHead+" "+pattern, http.HandlerFunc(handler))
}

// Post registers fn for POST requests matching pattern:
//
//	r.Post("/ping", h)
//
// is equivalent to:
//
//	r.Handle("POST /ping", h)
func (m *Mux) Post(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodPost+" "+pattern, http.HandlerFunc(handler))
}

// Put registers fn for PUT requests matching pattern:
//
//	r.Put("/ping", h)
//
// is equivalent to:
//
//	r.Handle("PUT /ping", h)
func (m *Mux) Put(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodPut+" "+pattern, http.HandlerFunc(handler))
}

// Patch registers fn for PATCH requests matching pattern:
//
//	r.Patch("/ping", h)
//
// is equivalent to:
//
//	r.Handle("PATCH /ping", h)
func (m *Mux) Patch(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodPatch+" "+pattern, http.HandlerFunc(handler))
}

// Delete registers fn for DELETE requests matching pattern:
//
//	r.Delete("/ping", h)
//
// is equivalent to:
//
//	r.Handle("DELETE /ping", h)
func (m *Mux) Delete(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodDelete+" "+pattern, http.HandlerFunc(handler))
}

// Connect registers fn for CONNECT requests matching pattern:
//
//	r.Connect("/ping", h)
//
// is equivalent to:
//
//	r.Handle("CONNECT /ping", h)
func (m *Mux) Connect(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodConnect+" "+pattern, http.HandlerFunc(handler))
}

// Options registers fn for OPTIONS requests matching pattern:
//
//	r.Options("/ping", h)
//
// is equivalent to:
//
//	r.Handle("OPTIONS /ping", h)
func (m *Mux) Options(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodOptions+" "+pattern, http.HandlerFunc(handler))
}

// Trace registers fn for TRACE requests matching pattern:
//
//	r.Trace("/ping", h)
//
// is equivalent to:
//
//	r.Handle("TRACE /ping", h)
func (m *Mux) Trace(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(http.MethodTrace+" "+pattern, http.HandlerFunc(handler))
}

// NotFound registers fn as the fallback for requests that match no route:
//
//	r.Get("/ping", h)
//	// GET /missing falls back to fn.
//
// A path that matches but a method that does not is MethodNotAllowed,
// not NotFound. Passing nil restores the default.
func (m *Mux) NotFound(handler http.HandlerFunc) {
	if m.inline {
		if handler != nil {
			handler = chain.New(m.mws...).Then(handler).ServeHTTP
		}
		m.parent.notFound = handler
		return
	}

	m.notFound = handler
}

// MethodNotAllowed registers fn for requests whose path matches a route
// but whose method does not:
//
//	r.Get("/ping", h)
//	// POST /ping falls back to fn, not NotFound.
//
// Passing nil restores the default 405 response.
func (m *Mux) MethodNotAllowed(handler http.HandlerFunc) {
	if m.inline {
		if handler != nil {
			handler = chain.New(m.mws...).Then(handler).ServeHTTP
		}
		m.parent.notAllowed = handler
		return
	}
	m.notAllowed = handler
}

// handle registers the dispatch handler on the owner trees, splitting
// an optional host prefix and applying the base path.
func (m *Mux) handle(method, pattern, original string, handler http.Handler) {
	if pattern == "" {
		panic(fmt.Errorf("mino: %w %q: a path beginning with '/' is required", errInvalidPattern, ""))
	}

	host, routePath := "", pattern
	if pattern[0] != '/' {
		i := strings.IndexByte(pattern, '/')
		if i < 0 {
			panic(fmt.Errorf("mino: %w %q: a path beginning with '/' is required", errInvalidPattern, pattern))
		}
		host, routePath = pattern[:i], pattern[i:]
		if strings.ContainsAny(host, "{} \t") {
			panic(fmt.Errorf("mino: %w %q in pattern %q: host patterns must be literal", errInvalidHost, host, pattern))
		}
	}
	pattern = original[:len(original)-len(routePath)] + m.withBase(routePath)
	matchPath := m.withBase(routePath)
	if m.conf.CleanPathRedirect && method != "" && method != http.MethodConnect && cleanPath(matchPath) != matchPath {
		panic(fmt.Errorf("mino: %w %q: non-CONNECT path cannot match with clean-path redirects enabled", errInvalidPattern, pattern))
	}
	if m.conf.EscapedPath {
		matchPath = escapedMatchPath(matchPath, true)
	}

	owner, dispatch := m.prepare(pattern, handler)

	for target := owner; target != nil; target = target.parent {
		ep := target.routeTree(host, method).addRoutePath(pattern, matchPath, dispatch, m.conf.EscapedPath)
		ep.internal = target != owner
		if host == "" || target.parent == nil {
			break
		}
		dispatch = target.entry
	}
}

// attach mounts a subrouter entry under prefix, compiling the
// inherited middlewares when the child was derived by With.
func (m *Mux) attach(prefix string, entry *ChainHandler) {
	owner := m
	if m.inline {
		owner = m.parent
		entry.compile(m.mws)
	}
	prefix = m.withBase(prefix)
	matchPath := prefix
	if m.conf.EscapedPath {
		matchPath = escapedMatchPath(prefix, true)
	}
	owner.routeTree("", allMethods).addSubRoutePath(prefix, matchPath, entry)
}

// prepare resolves the owner mux of a new route and wraps the
// handler with the derived middlewares of inline children.
func (m *Mux) prepare(pattern string, handler http.Handler) (*Mux, http.Handler) {
	if handler == nil {
		panic(fmt.Errorf("mino: %w for pattern %q", errNilHandler, pattern))
	}
	if h, ok := handler.(http.HandlerFunc); ok && h == nil {
		panic(fmt.Errorf("mino: %w for pattern %q", errNilHandler, pattern))
	}
	if !m.inline {
		return m, handler
	}
	entry := &ChainHandler{handler: handler}
	entry.compile(m.mws)
	return m.parent, entry
}

// stripPrefix dispatches to h with prefix removed from the
// decoded and escaped path, replying 404 on mismatch.
func stripPrefix(prefix string, h http.Handler) http.Handler {
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if requestPath == "" {
			requestPath = "/"
		}

		rawPath := ""
		if r.URL.RawPath != "" {
			if escaped := r.URL.EscapedPath(); escaped == r.URL.RawPath {
				rawPath = escaped[escapedPrefixEnd(escaped, len(prefix)):]
				if rawPath == "" {
					rawPath = "/"
				} else if len(rawPath) >= 3 && rawPath[0] == '%' && rawPath[1] == '2' && (rawPath[2] == 'F' || rawPath[2] == 'f') {
					// Ordinary matching may split at an encoded slash. In the delegated
					// URL that byte is now the leading path separator, not segment data.
					rawPath = "/" + rawPath[3:]
				}
			}
		}

		r2 := new(http.Request)
		*r2 = *r
		u := *r.URL
		u.Path = requestPath
		u.RawPath = rawPath
		r2.URL = &u
		h.ServeHTTP(w, r2)
	})
	return &ChainHandler{handler: h, chain: dispatch}
}

// normalizePrefix validates a grouping prefix and trims trailing
// slashes; it panics on empty, non-slash or wildcard prefixes.
func normalizePrefix(prefix string) string {
	if prefix == "" || prefix[0] != '/' {
		panic(fmt.Errorf("mino: %w %q: Route and Mount require a path beginning with '/'", errInvalidPrefix, prefix))
	}
	if strings.ContainsAny(prefix, "{}") {
		panic(fmt.Errorf("mino: %w %q: Route and Mount require a literal prefix without parameters", errInvalidPrefix, prefix))
	}
	if prefix = strings.TrimRight(prefix, "/"); prefix == "" {
		return "/"
	}
	return prefix
}

// routeTree returns the tree for host and method, creating it on
// first use; an empty host selects the global trees.
func (m *Mux) routeTree(host, method string) *node {
	trees := m.trees
	if host != "" {
		if m.hosts == nil {
			m.hosts = make(map[string]methodTrees)
		}
		trees = m.hosts[host]
		if trees == nil {
			trees = make(methodTrees)
			m.hosts[host] = trees
		}
	} else if trees == nil {
		trees = make(methodTrees)
		m.trees = trees
	}

	n := trees[method]
	if n == nil {
		n = new(node)
		trees[method] = n
	}
	return n
}

// withBase joins an absolute value onto the mux base path; callers pass
// values already validated by normalizePrefix or handle, so value starts
// with '/'.
func (m *Mux) withBase(value string) string {
	if m.basePath == "" {
		return value
	}
	if value == "/" {
		return m.basePath
	}
	return m.basePath + value
}

// cleanPath cleans literal separators and dots, preserving a trailing slash.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	trailing := strings.HasSuffix(p, "/")
	p = path.Clean(p)
	if trailing && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// escapedMatchPath gives equivalent escapes the same radix key without
// losing segment boundaries. The ordinary matcher never calls it.
func escapedMatchPath(p string, isPattern bool) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var key strings.Builder
	var changed bool
	for from := 0; from < len(p); {
		end := segmentEnd(p, from)
		segment := p[from:end]
		normalized := segment
		if !isPattern || !strings.HasPrefix(segment, "{") {
			normalized = escapedMatchSegment(segment)
		}
		if normalized != segment && !changed {
			changed = true
			key.Grow(len(p))
			key.WriteString(p[:from])
		}
		if changed {
			key.WriteString(normalized)
			if end < len(p) {
				key.WriteByte('/')
			}
		}
		from = end + 1
	}
	if !changed {
		return p
	}
	return key.String()
}

// escapedMatchSegment canonicalizes one segment's escapes into the
// matching key, keeping the structural %2F, %25 and %7B verbatim.
func escapedMatchSegment(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	// These canonical escapes already preserve the structural bytes. Avoid
	// decoding and rebuilding them just to obtain the same key.
	canonical := true
	for i := strings.IndexByte(p, '%'); i >= 0; {
		if i+2 >= len(p) || !canonicalTriplet(p[i+1], p[i+2]) {
			canonical = false
			break
		}
		next := strings.IndexByte(p[i+3:], '%')
		if next < 0 {
			break
		}
		i += 3 + next
	}
	if canonical {
		return p
	}
	decoded := unescapePath(p)
	if !strings.ContainsAny(decoded, "/%{") {
		return decoded
	}
	var key strings.Builder
	key.Grow(len(p))
	for i := 0; i < len(decoded); i++ {
		switch decoded[i] {
		case '/':
			key.WriteString("%2F")
		case '%':
			key.WriteString("%25")
		case '{':
			key.WriteString("%7B")
		default:
			key.WriteByte(decoded[i])
		}
	}
	return key.String()
}

// canonicalTriplet reports whether the two hex digits after '%' spell one of
// the structural escapes kept verbatim in radix keys: %2F, %25 or %7B.
func canonicalTriplet(hi, lo byte) bool {
	return (hi == '2' && (lo == 'F' || lo == '5')) || (hi == '7' && lo == 'B')
}

// unescapePath decodes percent escapes, returning p unchanged when
// the input is invalid.
func unescapePath(p string) string {
	if decoded, err := url.PathUnescape(p); err == nil {
		return decoded
	}
	return p
}

// redirectURL constructs a URL from an escaped path. Path holds the
// decoded form so String can validate and serialize it; RawPath
// preserves the original escaping when it is a valid encoding.
func redirectURL(escapedPath string) url.URL {
	return url.URL{Path: unescapePath(escapedPath), RawPath: escapedPath}
}

// escapedPrefixEnd maps a verified decoded prefix length back to its spelling.
// Each valid percent triplet represents one byte, including UTF-8 bytes.
// p is an EscapedPath result, so every '%' starts a complete triplet.
func escapedPrefixEnd(p string, n int) int {
	i := 0
	for n > 0 && i < len(p) {
		if p[i] == '%' {
			i += 3
		} else {
			i++
		}
		n--
	}
	return i
}

// pathView prepares optional path policies without changing the request.
// Callers bypass it entirely when both policies are disabled.
func (m *Mux) pathView(r *http.Request) (requestPath, targetPath string, clean bool) {
	requestPath = r.URL.Path
	escaped := r.URL.EscapedPath()
	targetPath = escaped
	if m.conf.CleanPathRedirect && r.Method != http.MethodConnect {
		targetPath = cleanPath(escaped)
		clean = targetPath != escaped
	}
	if m.conf.EscapedPath {
		requestPath = escapedMatchPath(targetPath, false)
	} else if clean {
		requestPath = unescapePath(targetPath)
	}
	return requestPath, targetPath, clean
}

// routeHTTP resolves protocol rules, optional normalization, matching and
// redirects before committing route metadata. Middleware always wraps this work.
func (m *Mux) routeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(m.trees) == 0 && len(m.hosts) == 0 && !m.conf.CleanPathRedirect {
		if r.Pattern != "" {
			clearRouteValues(r)
		}
		r.Pattern = ""
		m.notFoundHandler().ServeHTTP(w, r)
		return
	}

	connect := r.Method == http.MethodConnect
	requestPath, targetPath, clean := r.URL.Path, "", false
	if m.conf.EscapedPath || m.conf.CleanPathRedirect {
		requestPath, targetPath, clean = m.pathView(r)
	}
	if requestPath == "" && !connect {
		requestPath = "/"
	}

	host := ""
	if m.hosts != nil {
		host = requestHost(r)
	}
	var valuesBuf [16]string
	values := valuesBuf[:]
	// CONNECT has a separate slash probe host; it does not clean the path.
	if connect && m.connectRedirect(w, r, requestPath) {
		return
	}
	n, matched, exact := m.match(host, r.Method, requestPath, values)
	slashPath := ""
	// A Route/Mount entry delegates the request; the child owns its redirects.
	// A normal catch-all remains inexact and must allow a more specific slash probe.
	if clean || (!exact && (matched == nil || !matched.subroute)) {
		var redirected bool
		slashPath, redirected = m.redirectMatch(w, r, host, requestPath, targetPath, matched, clean, exact)
		if redirected {
			return
		}
	}
	if matched != nil {
		if len(matched.paramKeys) > len(values) {
			values = make([]string, len(matched.paramKeys))
			matched, _ = n.search(requestPath, values)
		}
		if r.Pattern != "" && r.Pattern != matched.pattern && strings.IndexByte(r.Pattern, '{') >= 0 {
			clearRouteValues(r)
		}
		for i, name := range matched.paramKeys {
			value := values[i]
			if m.conf.EscapedPath {
				value = unescapePath(value)
			}
			r.SetPathValue(name, value)
		}
		r.Pattern = matched.pattern
		matched.handler.ServeHTTP(w, r)
		return
	}
	clearRouteValues(r)
	r.Pattern = ""
	if m.conf.TrailingSlashRedirect && slashPath == "" && !strings.HasSuffix(requestPath, "/") {
		slashPath = requestPath + "/"
	}
	if allowed := m.matchingMethods(r, host, requestPath, slashPath); len(allowed) != 0 {
		w.Header().Set(allowHeader, strings.Join(allowed, allowMethodSeparator))
		m.methodNotAllowedHandler().ServeHTTP(w, r)
		return
	}
	m.notFoundHandler().ServeHTTP(w, r)
}

// redirectMatch handles inexact matches and pending normalization;
// probes do not write path parameters.
func (m *Mux) redirectMatch(w http.ResponseWriter, r *http.Request, host, requestPath, targetPath string, matched *endpoint, clean, exact bool) (string, bool) {
	slashPath, pattern, location := m.redirectTarget(r, host, requestPath, targetPath, matched, clean, exact)
	if location == "" {
		return slashPath, false
	}
	clearRouteValues(r)
	r.Pattern = pattern
	m.writeRedirect(w, r, location)
	return slashPath, true
}

// redirectTarget resolves slash and path-cleaning redirects without
// modifying the request. An empty location means no redirect.
func (m *Mux) redirectTarget(r *http.Request, host, requestPath, targetPath string, matched *endpoint, clean, exact bool) (slashPath, pattern, location string) {
	if !exact && r.Method != http.MethodConnect && m.conf.TrailingSlashRedirect && !strings.HasSuffix(requestPath, "/") {
		slashPath = requestPath + "/"
		if _, ep, full := m.match(host, r.Method, slashPath, nil); ep != nil && full {
			if targetPath == "" {
				targetPath = r.URL.EscapedPath()
			}
			return slashPath, ep.pattern, redirectLocation(redirectURL(targetPath+"/"), r.URL.RawQuery)
		}
	}
	if clean {
		if matched != nil {
			pattern = matched.pattern
		}
		// The cleaned escaped path goes out verbatim: RawPath preserves the
		// request's escaping instead of re-encoding it. The matching view
		// itself remains single-decoded.
		return slashPath, pattern, redirectLocation(redirectURL(targetPath), r.URL.RawQuery)
	}
	return slashPath, "", ""
}

// redirectHandler captures the decided location, not the query request.
func (m *Mux) redirectHandler(location string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.writeRedirect(w, r, location)
	})
}

// redirectLocation renders a decided target with the incoming raw query
// as a Location value; a leading double slash gains a ./ prefix to stay
// on this origin.
func redirectLocation(u url.URL, rawQuery string) string {
	u.RawQuery = rawQuery
	location := u.String()
	// A leading double slash would be a network-path reference. A dot prefix
	// keeps the redirect on this origin without cleaning the decided path.
	if strings.HasPrefix(location, "//") {
		location = "/." + location
	}
	return location
}

// redirectStatus validates an automatic redirect status, mapping zero
// to 307; unsupported codes panic.
func redirectStatus(code int) int {
	switch code {
	case 0:
		return http.StatusTemporaryRedirect
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return code
	default:
		panic(fmt.Errorf("mino: %w %d: use 301, 302, 303, 307 or 308", errInvalidRedirectStatus, code))
	}
}

// writeRedirect replies with the configured status and the already
// resolved location, mirroring http.Redirect's response shape without
// cleaning the target again. NewMuxWithConf normalizes the status, so
// conf.RedirectStatus is never zero here.
func (m *Mux) writeRedirect(w http.ResponseWriter, r *http.Request, location string) {
	code := m.conf.RedirectStatus
	// http.Redirect calls path.Clean even when normalization was disabled.
	// Keep its response format while writing the already-resolved target verbatim.
	header := w.Header()
	_, contentTypeSet := header["Content-Type"]
	header.Set("Location", escapeNonASCII(location))
	if !contentTypeSet && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		header.Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(code)
	if !contentTypeSet && r.Method == http.MethodGet {
		fmt.Fprintf(w, "<a href=\"%s\">%s</a>.\n\n", html.EscapeString(location), http.StatusText(code))
	}
}

// escapeNonASCII percent-escapes bytes outside ASCII, so Location stays
// header-safe, while RawQuery and its existing escapes stay intact.
func escapeNonASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < utf8.RuneSelf {
			continue
		}
		const hex = "0123456789abcdef"
		var escaped strings.Builder
		escaped.Grow(len(s) + 2)
		escaped.WriteString(s[:i])
		for ; i < len(s); i++ {
			b := s[i]
			if b < utf8.RuneSelf {
				escaped.WriteByte(b)
			} else {
				escaped.WriteByte('%')
				escaped.WriteByte(hex[b>>4])
				escaped.WriteByte(hex[b&15])
			}
		}
		return escaped.String()
	}
	return s
}

// connectRedirect probes URL.Host for a slash redirect on CONNECT
// requests and writes it when found; it reports whether a redirect
// response was sent.
func (m *Mux) connectRedirect(w http.ResponseWriter, r *http.Request, requestPath string) bool {
	if target := m.connectRedirectPath(r, requestPath); target != "" {
		clearRouteValues(r)
		r.Pattern = target
		m.writeRedirect(w, r, redirectLocation(url.URL{Path: target}, r.URL.RawQuery))
		return true
	}
	return false
}

// connectRedirectPath probes URL.Host; final CONNECT dispatch uses Request.Host.
func (m *Mux) connectRedirectPath(r *http.Request, requestPath string) string {
	if !m.conf.TrailingSlashRedirect || strings.HasSuffix(requestPath, "/") {
		return ""
	}
	_, _, exact := m.match(r.URL.Host, r.Method, requestPath, nil)
	if exact {
		return ""
	}
	if _, ep, full := m.match(r.URL.Host, r.Method, requestPath+"/", nil); ep != nil && full {
		return cleanPath(r.URL.Path) + "/"
	}
	return ""
}

// matchingMethods retains ServeMux's probe host for failed CONNECT lookups.
// CONNECT itself can still be allowed there, unlike an ordinary failed method.
func (m *Mux) matchingMethods(r *http.Request, host, requestPath, slashPath string) []string {
	excludeMethod := r.Method
	if r.Method == http.MethodConnect {
		host, excludeMethod = r.URL.Host, ""
	}
	return m.allowedMethods(host, excludeMethod, requestPath, slashPath)
}

// clearRouteValues removes only parameters named by a previous Mino match.
// Other values installed explicitly by application middleware are left alone.
func clearRouteValues(r *http.Request) {
	pattern := r.Pattern
	for from := strings.IndexByte(pattern, '{'); from >= 0; {
		pattern = pattern[from+1:]
		end := strings.IndexByte(pattern, '}')
		if end < 0 {
			return
		}
		name := strings.TrimSuffix(pattern[:end], "...")
		if name != "$" && validWildcardName(name) {
			r.SetPathValue(name, "")
		}
		pattern = pattern[end+1:]
		from = strings.IndexByte(pattern, '{')
	}
}

// match resolves the endpoint from host-scoped trees first, then
// from the global ones.
func (m *Mux) match(host, method, path string, values []string) (*node, *endpoint, bool) {
	if host != "" {
		if n, ep, full := matchTrees(m.hosts[host], method, path, values); ep != nil {
			return n, ep, full
		}
	}
	return matchTrees(m.trees, method, path, values)
}

// allowedMethods collects the methods that could still match
// requestPath for the Allow header; a registered GET allows HEAD.
func (m *Mux) allowedMethods(host, excludeMethod, requestPath, slashPath string) []string {
	allowed := make(map[string]struct{}, len(m.trees)+len(m.hosts[host])+1)

	collect := func(trees methodTrees) {
		for candidateMethod, n := range trees {
			if candidateMethod == allMethods || candidateMethod == excludeMethod {
				continue
			}
			ep, _ := n.search(requestPath, nil)
			if ep == nil && slashPath != "" {
				ep, _ = n.search(slashPath, nil)
			}
			if ep == nil {
				continue
			}
			allowed[candidateMethod] = struct{}{}
			if candidateMethod == http.MethodGet {
				allowed[http.MethodHead] = struct{}{}
			}
		}
	}
	if host != "" {
		collect(m.hosts[host])
	}
	collect(m.trees)

	if len(allowed) == 0 {
		return nil
	}
	methods := make([]string, 0, len(allowed))
	for allowedMethod := range allowed {
		methods = append(methods, allowedMethod)
	}
	sort.Strings(methods)
	return methods
}

// notFoundHandler walks up the mount chain for a NotFound override,
// falling back to http.NotFound.
func (m *Mux) notFoundHandler() http.HandlerFunc {
	for current := m; current != nil; current = current.parent {
		if current.notFound != nil {
			return current.notFound
		}
	}
	return http.NotFound
}

// methodNotAllowedHandler walks up the mount chain for a 405
// override, falling back to the plain 405 reply.
func (m *Mux) methodNotAllowedHandler() http.HandlerFunc {
	for current := m; current != nil; current = current.parent {
		if current.notAllowed != nil {
			return current.notAllowed
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(
			w,
			http.StatusText(http.StatusMethodNotAllowed),
			http.StatusMethodNotAllowed,
		)
	}
}

// matchTrees searches trees for method, falling back from HEAD to
// GET and finally to the any-method tree.
func matchTrees(trees methodTrees, method, path string, values []string) (*node, *endpoint, bool) {
	if n := trees[method]; n != nil {
		if ep, full := n.search(path, values); ep != nil {
			return n, ep, full
		}
	}
	if method == http.MethodHead {
		if n := trees[http.MethodGet]; n != nil {
			if ep, full := n.search(path, values); ep != nil {
				return n, ep, full
			}
		}
	}
	if n := trees[allMethods]; n != nil {
		if ep, full := n.search(path, values); ep != nil {
			return n, ep, full
		}
	}
	return nil, nil, false
}

// validMethod reports whether method is a valid HTTP token.
func validMethod(method string) bool {
	if method == "" {
		return false
	}

	for i := 0; i < len(method); i++ {
		c := method[i]
		if c >= '0' && c <= '9' ||
			c >= 'A' && c <= 'Z' ||
			c >= 'a' && c <= 'z' {
			continue
		}

		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}

	return true
}

// requestHost strips the port from the Host header, keeping the
// raw value for CONNECT requests.
func requestHost(r *http.Request) string {
	host := r.Host
	if r.Method == http.MethodConnect || strings.IndexByte(host, ':') < 0 {
		return host
	}
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return host
}

// A ChainHandler is an http.Handler that serves requests through a
// pre-compiled chain of middlewares.
type ChainHandler struct {
	handler http.Handler
	chain   http.Handler
	mws     []chain.Middleware
}

// ServeHTTP serves the request through the compiled middleware chain.
func (h *ChainHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.chain.ServeHTTP(w, r)
}

// compile builds the middleware chain around the final handler and
// snapshots the middlewares for introspection.
func (h *ChainHandler) compile(mws []chain.Middleware) {
	h.chain = chain.New(mws...).Then(h.handler)
	h.mws = append([]chain.Middleware(nil), mws...)
}
