package httpx

import (
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/hainuoli/mino/core/errorx"
	"github.com/hainuoli/mino/core/jsonx"
	"github.com/hainuoli/mino/core/mapping"
)

// maxJSONBody is the default JSON body limit: 8 MiB.
const maxJSONBody = 8 << 20

type (
	// ParseOption configures JSON body parsing for one Parse or ParseJSON call.
	// Nil options are ignored; later options override earlier ones.
	ParseOption func(*parseOptions)

	parseOptions struct{ maxBody int64 }
)

// WithMaxJSONBody sets the JSON body limit in bytes for this call only.
// The default is 8 MiB. Zero allows only an empty body; negative limits return
// a configuration error before reading the body. Forms retain net/http limits.
//
//	err := httpx.Parse(r, &req, httpx.WithMaxJSONBody(32<<20))
func WithMaxJSONBody(n int64) ParseOption {
	return func(opt *parseOptions) {
		opt.maxBody = n
	}
}

// Parse binds a request into a non-nil pointer and then calls Validate() error
// if v implements it. Struct targets receive JSON, form, query, header, and path
// values in that order; other targets, including slices and maps, receive only
// JSON. Present values from later sources overwrite earlier values.
//
// JSON is selected by application/json or a +json Content-Type, including when
// Content-Length is unknown; missing or other media types skip it and leave the
// body to the corresponding parser or handler. Use ParseJSON to decode a JSON
// body regardless of Content-Type, without automatic validation. JSON fields
// follow the configured jsonx codec; with the default codec, fields without json
// tags also accept JSON, so use json:"-" on fields reserved for other sources.
//
// Missing inputs keep existing values, and parsing never closes the body. On
// failure, v may be partially populated and should not be used. opts configure
// the JSON body limit for this call only; see WithMaxJSONBody.
//
//	type req struct {
//		Name string `json:"name"`
//		Page int    `query:"page" json:"-"`
//	}
//
//	var in req
//	err := httpx.Parse(r, &in)
//	// in.Page -> 2 for the query "?page=2"
func Parse(r *http.Request, v any, opts ...ParseOption) error {
	target, err := mapping.Deref(v)
	if err != nil {
		return err
	}
	options, err := makeParseOptions(opts)
	if err != nil {
		return err
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return requestFailure(http.StatusBadRequest, "invalid Content-Type",
				errorx.Wrap(err, "parse Content-Type"))
		}
		if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
			if err := parseJSON(r, v, options.maxBody); err != nil {
				return err
			}
		}
	}

	if target.Kind() == reflect.Struct {
		if err := ParseForm(r, v); err != nil {
			return err
		}
		if err := ParseQuery(r, v); err != nil {
			return err
		}
		if err := ParseHeader(r, v); err != nil {
			return err
		}
		if err := ParsePath(r, v); err != nil {
			return err
		}
	}
	if validator, ok := v.(interface{ Validate() error }); ok {
		return requestFailure(http.StatusBadRequest, "invalid request",
			errorx.Wrap(validator.Validate(), "validate request"))
	}
	return nil
}

// ParseJSON decodes one complete JSON document into a non-nil pointer using
// core/jsonx, regardless of Content-Type. It does not bind other sources or run
// Validate; use Parse for automatic source selection and validation.
// Empty and absent bodies are allowed. The default limit is 8 MiB;
// WithMaxJSONBody changes it for this call. Parsing does not close the body.
//
//	var user struct {
//		Name string `json:"name"`
//	}
//	err := httpx.ParseJSON(r, &user)
//	// user.Name -> "Hainuo" for the body {"name":"Hainuo"}
func ParseJSON(r *http.Request, v any, opts ...ParseOption) error {
	if _, err := mapping.Deref(v); err != nil {
		return err
	}
	options, err := makeParseOptions(opts)
	if err != nil {
		return err
	}
	return parseJSON(r, v, options.maxBody)
}

// ParsePath binds path-tagged fields using http.Request.PathValue.
//
//	var in struct {
//		ID int `path:"id"`
//	}
//	err := httpx.ParsePath(r, &in)
//	// in.ID -> 7 for the route /users/{id} and the URL /users/7
func ParsePath(r *http.Request, v any) error {
	return bind(v, "path", func(name string) []string {
		if value := r.PathValue(name); value != "" {
			return []string{value}
		}
		return nil
	})
}

// ParseQuery binds query-tagged fields from the URL query.
//
//	var in struct {
//		Page int `query:"page"`
//	}
//	err := httpx.ParseQuery(r, &in)
//	// in.Page -> 2 for the query "?page=2"
func ParseQuery(r *http.Request, v any) error {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return requestFailure(http.StatusBadRequest, "invalid query parameters",
			errorx.Wrap(err, "parse query"))
	}
	return bind(v, "query", func(name string) []string { return values[name] })
}

// ParseForm binds form-tagged fields from r.Form, which combines query and
// URL-encoded form values. Multipart form data must be parsed separately.
// Form parsing runs before destination binding; input errors take precedence
// over invalid destinations.
//
//	var in struct {
//		Page int `form:"page"`
//	}
//	err := httpx.ParseForm(r, &in)
//	// in.Page -> 2 for the posted form "page=2"
func ParseForm(r *http.Request, v any) error {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	if err := r.ParseForm(); err != nil {
		return requestFailure(http.StatusBadRequest, "invalid form data",
			bodyFailure(errorx.Wrap(err, "parse form")))
	}
	return bind(v, "form", func(name string) []string { return r.Form[name] })
}

// ParseHeader binds header-tagged fields using case-insensitive header names.
//
//	var in struct {
//		Token string `header:"Authorization"`
//	}
//	err := httpx.ParseHeader(r, &in)
//	// in.Token -> "Bearer jwt" for the Authorization header
func ParseHeader(r *http.Request, v any) error {
	return bind(v, "header", r.Header.Values)
}

// makeParseOptions applies opts over the default limit and rejects negative limits.
func makeParseOptions(opts []ParseOption) (parseOptions, error) {
	options := parseOptions{maxBody: maxJSONBody}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.maxBody < 0 {
		return parseOptions{}, fmt.Errorf("httpx: max JSON body must be non-negative, got %d", options.maxBody)
	}
	return options, nil
}

// parseJSON reads and decodes the JSON body within maxBody bytes, leaving the
// body open and treating an empty body as absent.
func parseJSON(r *http.Request, v any, maxBody int64) error {
	if r.Body == nil {
		return nil
	}

	limit := maxBody
	// Read one extra byte to detect overflow without trusting Content-Length.
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return bodyFailure(errorx.Wrap(err, "read JSON body"))
	}
	if int64(len(data)) > maxBody {
		return requestFailure(http.StatusRequestEntityTooLarge, "request body too large",
			fmt.Errorf("JSON body exceeds %d bytes", maxBody))
	}
	if len(data) == 0 {
		return nil
	}
	return requestFailure(http.StatusBadRequest, "invalid request body",
		errorx.Wrap(jsonx.Unmarshal(data, v), "decode JSON body"))
}

// bind adapts mapping failures to request errors, passing
// mapping.ErrInvalidTarget through unchanged.
func bind(v any, tag string, lookup func(string) []string) error {
	err := mapping.Bind(v, tag, lookup)
	if errors.Is(err, mapping.ErrInvalidTarget) {
		return err
	}
	return requestFailure(http.StatusBadRequest, "invalid request parameters", err)
}
