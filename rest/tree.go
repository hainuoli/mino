package rest

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/hainuoli/mino/rest/chain"
)

const (
	// ntStatic is a plain prefix segment.
	ntStatic nodeTyp = iota
	// ntParam is a {name} segment matching exactly one path segment.
	ntParam
	// ntCatchAll is a {name...} segment matching everything to the end.
	ntCatchAll
	// ntEscapedParam excludes a segment decoding to a slash, like ServeMux.
	ntEscapedParam
)

var (
	errInvalidPattern = errors.New("invalid pattern")
	errDuplicate      = errors.New("duplicate route")
)

type (
	// nodeTyp classifies tree segments: static prefixes, {name}
	// parameters and {name...} catch-alls.
	nodeTyp uint8

	// node is a radix tree node sharing common prefixes with its
	// siblings; static children live in a sparse array indexed by
	// their first byte. Each terminal maps HTTP methods to endpoints.
	node struct {
		prefix    string
		endpoints map[string]*endpoint
		typ       nodeTyp
		base      byte

		children []*node
		param    *node
		catchAll *node
	}

	// endpoint is the leaf of a registered pattern: the dispatch
	// handler plus everything introspection needs to describe it.
	endpoint struct {
		handler   http.Handler
		pattern   string
		paramKeys []string
		// internal marks matching-only entries, such as host delegates and
		// escaped-path aliases; route inspection hides them.
		internal bool
		// subroute marks the entry endpoint of a mounted subtree.
		subroute bool
	}
)

// addChild inserts child into the sparse children array, re-basing
// it when the key range grows.
func (n *node) addChild(c byte, child *node) {
	if len(n.children) == 0 {
		n.base = c
		n.children = []*node{child}
		return
	}

	base, key := int(n.base), int(c)
	end := base + len(n.children)
	lo := min(base, key)
	hi := max(end, key+1)

	if lo != base || hi != end {
		children := make([]*node, hi-lo)
		copy(children[base-lo:], n.children)
		n.children = children
		n.base = byte(lo)
	}

	n.children[key-lo] = child
}

// addEndpoint inserts an already parsed endpoint at its matching path.
func (n *node) addEndpoint(method, path string, ep *endpoint, escaped bool) *endpoint {
	from := 0
	for i := 1; i <= len(path); {
		end := segmentEnd(path, i)
		seg := path[i:end]
		if seg == "{$}" {
			return n.insert(path[from:i]).setEndpoint(method, ep)
		}
		typ, _ := parseSegment(ep.pattern, seg, end == len(path))
		if typ != ntStatic {
			if escaped && typ == ntParam {
				typ = ntEscapedParam
			}
			n = n.insert(path[from:i]).wildChild(typ)
			if typ == ntCatchAll || end == len(path) {
				return n.setEndpoint(method, ep)
			}
			from = end
		}
		if end == len(path) {
			break
		}
		i = end + 1
	}
	n = n.insert(path[from:])
	if path[len(path)-1] == '/' {
		n = n.wildChild(ntCatchAll)
	}
	return n.setEndpoint(method, ep)
}

// addRoutePath separates the original pattern from its internal matching key.
func (n *node) addRoutePath(method, pattern, path string, handler http.Handler, escaped bool) *endpoint {
	ep := &endpoint{handler: handler, pattern: pattern, paramKeys: parsePattern(pattern)}
	if escaped {
		// ServeMux treats {$} and a decoded trailing slash as the same
		// key; register both spellings. Only the original entry is public.
		aliasPath := ""
		switch {
		case strings.HasSuffix(path, "/{$}"):
			aliasPath = strings.TrimSuffix(path, "{$}") + "%2F"
		case strings.HasSuffix(path, "/%2F"):
			aliasPath = strings.TrimSuffix(path, "%2F") + "{$}"
		}
		if aliasPath != "" {
			alias := *ep
			alias.internal = true
			n.addEndpoint(method, aliasPath, &alias, escaped)
		}
	}
	return n.addEndpoint(method, path, ep, escaped)
}

// addSubRoutePath keeps mount metadata independent of the matching key.
// Callers pass a prefix already normalized to start with '/'.
func (n *node) addSubRoutePath(prefix, matchPath string, handler http.Handler) {
	exact := n.insert(matchPath)
	subtree := exact
	subtreePattern := prefix

	if prefix != "/" {
		subtreePattern += "/"
		subtree = n.insert(matchPath + "/")
	}

	var existing *endpoint
	if prefix != "/" {
		existing = exact.endpoints[allMethods]
	}
	if existing == nil && subtree.catchAll != nil {
		existing = subtree.catchAll.endpoints[allMethods]
	}
	if existing != nil {
		panic(fmt.Errorf("mino: %w at mount prefix %q: conflicts with registered pattern %q", errDuplicate, prefix, existing.pattern))
	}

	entry := &endpoint{handler: handler, pattern: prefix, subroute: true}
	if prefix == "/" {
		// The root catch-all already matches both / and its descendants.
		subtree.wildChild(ntCatchAll).setEndpoint(allMethods, entry)
		return
	}
	exact.setEndpoint(allMethods, entry)
	subtree.wildChild(ntCatchAll).setEndpoint(allMethods, &endpoint{
		handler:  handler,
		pattern:  subtreePattern,
		internal: true,
		subroute: true,
	})
}

// child returns the static child covering byte c, or nil.
func (n *node) child(c byte) *node {
	i := int(c) - int(n.base)
	if uint(i) < uint(len(n.children)) {
		return n.children[i]
	}
	return nil
}

// insert walks or creates the nodes for prefix, splitting existing
// nodes at their common prefix, and returns the deepest node.
func (n *node) insert(prefix string) *node {
	if prefix == "" {
		return n
	}
	if n.typ != ntStatic {
		if child := n.child(prefix[0]); child != nil {
			return child.insert(prefix)
		}
		child := &node{prefix: prefix}
		n.addChild(prefix[0], child)
		return child
	}
	if n.prefix == "" && len(n.endpoints) == 0 && len(n.children) == 0 && n.param == nil && n.catchAll == nil {
		n.prefix = prefix
		return n
	}
	for {
		common := commonPrefix(prefix, n.prefix)
		if common < len(n.prefix) {
			child := new(node)
			*child = *n
			child.prefix = n.prefix[common:]
			*n = node{prefix: n.prefix[:common]}
			n.addChild(child.prefix[0], child)
			if common == len(prefix) {
				return n
			}
			child = &node{prefix: prefix[common:]}
			n.addChild(child.prefix[0], child)
			return child
		}
		if common == len(prefix) {
			return n
		}
		prefix = prefix[common:]
		if child := n.child(prefix[0]); child != nil {
			n = child
			continue
		}
		child := &node{prefix: prefix}
		n.addChild(child.prefix[0], child)
		return child
	}
}

// match walks the tree recursively: static children win, then the
// parameter child, then the catch-all. A branch without a handler for the
// method backtracks; parameter values are written into values at their depth.
func (n *node) match(method, path string, values []string, depth int) (*endpoint, bool) {
	for {
		if len(path) < len(n.prefix) || len(n.prefix) > 1 && path[1:len(n.prefix)] != n.prefix[1:] {
			return nil, false
		}
		path = path[len(n.prefix):]
		if path == "" {
			if ep := n.endpoint(method); ep != nil {
				return ep, true
			}
			if catchAll := n.catchAll; catchAll != nil {
				if ep := catchAll.endpoint(method); ep != nil {
					if depth < len(ep.paramKeys) && depth < len(values) {
						values[depth] = ""
					}
					return ep, true
				}
			}
			return nil, false
		}
		if next := n.child(path[0]); next != nil {
			if n.param == nil && n.catchAll == nil {
				n = next
				continue
			}
			if ep, full := next.match(method, path, values, depth); ep != nil {
				return ep, full
			}
		}
		if param := n.param; param != nil {
			end := segmentEnd(path, 0)
			if end > 0 && (param.typ != ntEscapedParam || path[:end] != "%2F") {
				if depth < len(values) {
					values[depth] = path[:end]
				}
				if end == len(path) {
					if ep := param.endpoint(method); ep != nil {
						return ep, true
					}
				} else if len(param.children) != 0 {
					next := param.children[0]
					if n.catchAll == nil {
						n, path, depth = next, path[end:], depth+1
						continue
					}
					if ep, full := next.match(method, path[end:], values, depth+1); ep != nil {
						return ep, full
					}
				}
			}
		}
		if catchAll := n.catchAll; catchAll != nil {
			if ep := catchAll.endpoint(method); ep != nil {
				if depth < len(ep.paramKeys) && depth < len(values) {
					values[depth] = path
				}
				return ep, false
			}
		}
		return nil, false
	}
}

// routes collects every visible endpoint into Route snapshots,
// skipping internal endpoints and unwrapping mounted subtrees into
// SubRoutes. Patterns are reported relative to basePath.
func (n *node) routes(host, basePath string) []Route {
	var routes []Route

	var visit func(*node)
	visit = func(n *node) {
		if n == nil {
			return
		}

		for method, ep := range n.endpoints {
			if ep.internal {
				continue
			}
			handler, mws := routeHandler(ep.handler)

			var subRoutes Routes
			if ep.subroute {
				subRoutes, _ = handler.(Routes)
				if subRoutes != nil {
					handler = nil
				}
			}

			routes = append(routes, Route{
				Method:      method,
				Host:        host,
				Pattern:     patternToPath(ep.pattern, basePath),
				Handler:     handler,
				Middlewares: mws,
				SubRoutes:   subRoutes,
				Mount:       ep.subroute,
			})
		}

		for _, child := range n.children {
			visit(child)
		}
		visit(n.param)
		visit(n.catchAll)
	}

	visit(n)
	return routes
}

// search matches path against the tree, filling values with the
// parameter values in declaration order. The second result reports
// a full match, as opposed to a catch-all prefix match.
func (n *node) search(method, path string, values []string) (*endpoint, bool) {
	if n == nil || path == "" || path[0] != '/' {
		return nil, false
	}
	return n.match(method, path, values, 0)
}

// setEndpoint attaches ep for method, panicking on duplicate method/path pairs.
func (n *node) setEndpoint(method string, ep *endpoint) *endpoint {
	if existing := n.endpoints[method]; existing != nil {
		panic(fmt.Errorf("mino: %w %q: conflicts with registered pattern %q", errDuplicate, ep.pattern, existing.pattern))
	}
	if n.endpoints == nil {
		n.endpoints = make(map[string]*endpoint)
	}
	n.endpoints[method] = ep
	return ep
}

// endpoint selects a method at this path. HEAD falls back to GET, then
// both fall back to an any-method handler at the same terminal.
func (n *node) endpoint(method string) *endpoint {
	if ep := n.endpoints[method]; ep != nil {
		return ep
	}
	if method == http.MethodHead {
		if ep := n.endpoints[http.MethodGet]; ep != nil {
			return ep
		}
	}
	return n.endpoints[allMethods]
}

// matchingMethods visits every path branch that can match, collecting method
// keys from its terminals. Like search, it keeps escaped slashes in their segment.
func (n *node) matchingMethods(path, exclude string, allowed map[string]struct{}) {
	if n == nil || path == "" || path[0] != '/' {
		return
	}
	n.collectMethods(path, exclude, allowed)
}

func (n *node) collectMethods(path, exclude string, allowed map[string]struct{}) {
	if len(path) < len(n.prefix) || len(n.prefix) > 1 && path[1:len(n.prefix)] != n.prefix[1:] {
		return
	}
	path = path[len(n.prefix):]
	if path == "" {
		n.allowMethods(exclude, allowed)
	} else {
		if next := n.child(path[0]); next != nil {
			next.collectMethods(path, exclude, allowed)
		}
		if param := n.param; param != nil {
			end := segmentEnd(path, 0)
			if end > 0 && (param.typ != ntEscapedParam || path[:end] != "%2F") {
				if end == len(path) {
					param.allowMethods(exclude, allowed)
				} else if len(param.children) != 0 {
					param.children[0].collectMethods(path[end:], exclude, allowed)
				}
			}
		}
	}
	if n.catchAll != nil {
		n.catchAll.allowMethods(exclude, allowed)
	}
}

func (n *node) allowMethods(exclude string, allowed map[string]struct{}) {
	for method := range n.endpoints {
		if method == allMethods || method == exclude {
			continue
		}
		allowed[method] = struct{}{}
		if method == http.MethodGet {
			allowed[http.MethodHead] = struct{}{}
		}
	}
}

// wildChild returns the parameter or catch-all child, creating it
// on first use.
func (n *node) wildChild(typ nodeTyp) *node {
	if typ == ntParam || typ == ntEscapedParam {
		if n.param == nil {
			n.param = &node{typ: typ}
		}
		return n.param
	}
	if n.catchAll == nil {
		n.catchAll = &node{typ: typ}
	}
	return n.catchAll
}

// commonPrefix returns the length of the longest common prefix
// of a and b.
func commonPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// invalidSegment keeps registration diagnostics contextual without changing
// the parser's matching rules or introducing a request-time validation path.
func invalidSegment(pattern, segment, reason string) error {
	return fmt.Errorf("mino: %w %q: segment %q %s", errInvalidPattern, pattern, segment, reason)
}

// parsePattern extracts the wildcard names of a path in order;
// duplicates panic. Registration guarantees pattern contains '/', so
// the method and host prefix end at the first slash.
func parsePattern(pattern string) []string {
	var names []string
	path := pattern[strings.IndexByte(pattern, '/'):]

	for i := 1; i < len(path); {
		end := segmentEnd(path, i)

		if typ, name := parseSegment(pattern, path[i:end], end == len(path)); typ != ntStatic {
			if slices.Contains(names, name) {
				panic(fmt.Errorf("mino: %w %q: parameter %q is repeated", errInvalidPattern, pattern, name))
			}
			names = append(names, name)
		}

		i = end + 1
	}

	return names
}

// parseSegment classifies one path segment: {$} an exact-slash pin
// (last only), {name} a parameter, {name...} a catch-all (last only).
func parseSegment(pattern, segment string, last bool) (nodeTyp, string) {
	if segment == "{$}" {
		if !last {
			panic(invalidSegment(pattern, segment, "must be the final segment"))
		}
		return ntStatic, ""
	}
	if !strings.ContainsRune(segment, '{') {
		return ntStatic, ""
	}
	if len(segment) < 3 || segment[0] != '{' || segment[len(segment)-1] != '}' {
		panic(invalidSegment(pattern, segment, "must be a whole-segment parameter such as {name} or {name...}"))
	}
	name, typ := segment[1:len(segment)-1], ntParam
	if strings.HasSuffix(name, "...") {
		if !last {
			panic(invalidSegment(pattern, segment, "is a catch-all and must be the final segment"))
		}
		name, typ = name[:len(name)-3], ntCatchAll
	}
	if !validWildcardName(name) {
		panic(invalidSegment(pattern, segment, "has an invalid parameter name; use a letter or underscore first, followed by letters, digits, or underscores"))
	}
	return typ, name
}

// patternToPath strips the method and host prefix from a stored
// pattern and re-applies basePath. Stored patterns always contain '/'.
func patternToPath(pattern, basePath string) string {
	path := pattern[strings.IndexByte(pattern, '/'):]
	if basePath == "" {
		return path
	}
	base := strings.TrimSuffix(basePath, "/")
	if path == base {
		return "/"
	}
	if strings.HasPrefix(path, base+"/") {
		return path[len(base):]
	}
	return path
}

// routeHandler unwraps nested ChainHandlers, collecting their
// middlewares outermost first.
func routeHandler(handler http.Handler) (http.Handler, []chain.Middleware) {
	var mws []chain.Middleware
	for {
		ch, ok := handler.(*ChainHandler)
		if !ok {
			return handler, mws
		}
		mws = append(mws, ch.mws...)
		handler = ch.handler
	}
}

// segmentEnd returns the index of the next '/' starting at i.
func segmentEnd(p string, i int) int {
	for i < len(p) && p[i] != '/' {
		i++
	}
	return i
}

// validWildcardName reports whether s is a valid wildcard name:
// letters or underscore first, digits allowed afterwards.
func validWildcardName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !unicode.IsLetter(r) && r != '_' && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}
