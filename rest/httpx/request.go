package httpx

import (
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/hainuoli/mino/core/errorx"
	"github.com/hainuoli/mino/core/jsonx"
	"github.com/hainuoli/mino/core/mapping"
)

const (
	// maxJSONBody is the default JSON body limit: 8 MiB.
	maxJSONBody = 8 << 20
	// multipartMemory is the default in-memory file content threshold: 32 MiB.
	multipartMemory = 32 << 20
)

var (
	fileHeaderType  = reflect.TypeFor[*multipart.FileHeader]()
	fileHeadersType = reflect.TypeFor[[]*multipart.FileHeader]()
)

type (
	// ParseOption configures one Parse, ParseJSON, or ParseForm call.
	// Nil options are ignored; later options override earlier ones.
	ParseOption func(*parseOptions)

	parseOptions struct {
		maxJSONBody     int64
		multipartMemory int64
	}
)

// WithMaxJSONBody sets the JSON body limit in bytes for one parse call.
// The default is 8 MiB; zero allows only an empty body, and negative values
// fail before the body is read.
//
//	err := httpx.Parse(r, &req, httpx.WithMaxJSONBody(5<<20))
func WithMaxJSONBody(n int64) ParseOption {
	return func(opt *parseOptions) {
		opt.maxJSONBody = n
	}
}

// WithMultipartMemory sets the multipart in-memory file threshold for one
// parse call. The default is 32 MiB; zero stores nonempty file contents on
// disk, and negative values are configuration errors. It is not a request
// size limit; cap the body separately with http.MaxBytesReader or middleware.
//
//	err := httpx.ParseForm(r, &in, httpx.WithMultipartMemory(5<<20))
func WithMultipartMemory(n int64) ParseOption {
	return func(opt *parseOptions) {
		opt.multipartMemory = n
	}
}

// Parse binds a request into v and runs Validate() error if v implements it.
// JSON, form, query, header, and path values bind in that order, and present
// values from later sources overwrite earlier ones. JSON applies only to an
// application/json or +json Content-Type (ParseJSON ignores the header), and
// form binding requires a struct target. With the default codec, JSON also
// fills fields without a json tag, so reserve fields with json:"-".
// Parsing never closes the body and may leave v partially populated on
// failure; the caller cleans up multipart files (see ParseForm).
//
//	type req struct {
//		Name string `json:"name"`
//		Page int    `query:"page"`
//	}
//
//	var in req
//	err := httpx.Parse(r, &in)
//	// in -> {Name:"Hainuo", Page:2} for body {"name":"Hainuo"} and query "?page=2"
func Parse(r *http.Request, v any, opts ...ParseOption) error {
	target, err := mapping.Deref(v)
	if err != nil {
		return err
	}
	options, err := makeParseOptions(opts)
	if err != nil {
		return err
	}
	mediaType, err := mediaTypeOf(r)
	if err != nil {
		return err
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		if err := bindJSON(r, v, options.maxJSONBody); err != nil {
			return err
		}
	}

	if target.Kind() == reflect.Struct {
		if err := bindForm(r, v, options.multipartMemory); err != nil {
			return err
		}
		for _, parse := range [...]func(*http.Request, any) error{ParseQuery, ParseHeader, ParsePath} {
			if err := parse(r, v); err != nil {
				return err
			}
		}
	}
	if validator, ok := v.(interface{ Validate() error }); ok {
		return wrapRequestError(http.StatusBadRequest, "invalid request",
			errorx.Wrap(validator.Validate(), "validate request"))
	}
	return nil
}

// ParseJSON decodes the JSON body into a non-nil pointer, regardless of
// Content-Type. It does not bind other sources or run Validate; use Parse for
// both. An empty body is allowed, the default limit is 8 MiB (WithMaxJSONBody
// changes it for this call), and parsing never closes the body.
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
	return bindJSON(r, v, options.maxJSONBody)
}

// ParsePath binds path-tagged fields using http.Request.PathValue.
//
//	var in struct {
//		ID int `path:"id"`
//	}
//	err := httpx.ParsePath(r, &in)
//	// in.ID -> 7 for the route /users/{id} and the URL /users/7
func ParsePath(r *http.Request, v any) error {
	return wrapBindingError(mapping.Bind(v, "path", func(name string) []string {
		if value := r.PathValue(name); value != "" {
			return []string{value}
		}
		return nil
	}))
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
		return wrapRequestError(http.StatusBadRequest, "invalid query parameters",
			errorx.Wrap(err, "parse query"))
	}
	return wrapBindingError(mapping.Bind(v, "query", func(name string) []string { return values[name] }))
}

// ParseForm binds form-tagged fields from r.Form, combining query, URL-encoded,
// and multipart values with net/http precedence. File fields bind to
// *multipart.FileHeader or []*multipart.FileHeader by the same form tag and
// never from query or text values. The caller must close opened files and
// remove multipart temporary files, including on failure:
//
//	defer func() {
//	    if r.MultipartForm != nil { _ = r.MultipartForm.RemoveAll() }
//	}()
//
// ParseForm does not validate; Parse runs Validate after binding all sources.
//
//	var in struct {
//		Page int `form:"page"`
//	}
//	err := httpx.ParseForm(r, &in)
//	// in.Page -> 2 for the posted form "page=2"
func ParseForm(r *http.Request, v any, opts ...ParseOption) error {
	options, err := makeParseOptions(opts)
	if err != nil {
		return err
	}
	return bindForm(r, v, options.multipartMemory)
}

// ParseHeader binds header-tagged fields using case-insensitive header names.
//
//	var in struct {
//		Token string `header:"Authorization"`
//	}
//	err := httpx.ParseHeader(r, &in)
//	// in.Token -> "Bearer jwt" for the Authorization header
func ParseHeader(r *http.Request, v any) error {
	return wrapBindingError(mapping.Bind(v, "header", r.Header.Values))
}

// makeParseOptions applies opts over the defaults and rejects negative limits.
func makeParseOptions(opts []ParseOption) (parseOptions, error) {
	options := parseOptions{maxJSONBody: maxJSONBody, multipartMemory: multipartMemory}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.maxJSONBody < 0 {
		return parseOptions{}, fmt.Errorf("httpx: max JSON body must be non-negative, got %d", options.maxJSONBody)
	}
	if options.multipartMemory < 0 {
		return parseOptions{}, fmt.Errorf("httpx: multipart memory must be non-negative, got %d", options.multipartMemory)
	}
	return options, nil
}

// bindJSON decodes the JSON body into v within maxBody bytes, leaving the body
// open and treating an empty body as absent.
func bindJSON(r *http.Request, v any, maxBody int64) error {
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
		return wrapBodyError(errorx.Wrap(err, "read JSON body"))
	}
	if int64(len(data)) > maxBody {
		return wrapRequestError(http.StatusRequestEntityTooLarge, "request body too large",
			fmt.Errorf("JSON body exceeds %d bytes", maxBody))
	}
	if len(data) == 0 {
		return nil
	}
	return wrapRequestError(http.StatusBadRequest, "invalid request body",
		errorx.Wrap(jsonx.Unmarshal(data, v), "decode JSON body"))
}

// bindForm reads form data and binds text values and uploaded files to v.
func bindForm(r *http.Request, v any, memory int64) error {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	if err := r.ParseForm(); err != nil {
		return wrapRequestError(http.StatusBadRequest, "invalid form data", wrapBodyError(errorx.Wrap(err, "parse form")))
	}
	mediaType, err := mediaTypeOf(r)
	if err != nil {
		return err
	}
	if mediaType == "multipart/form-data" {
		body := &multipartBody{ReadCloser: r.Body}
		// Restore the original body before binding, including on panic.
		err := func() error {
			r.Body = body
			defer func() { r.Body = body.ReadCloser }()
			return r.ParseMultipartForm(memory)
		}()
		if err != nil {
			var storageErr *os.PathError
			switch {
			case body.err != nil:
				return wrapBodyError(errorx.Wrap(body.err, "read multipart body"))
			case errors.As(err, &storageErr):
				return errorx.Wrap(err, "store multipart form")
			case errors.Is(err, multipart.ErrMessageTooLarge):
				return wrapRequestError(http.StatusRequestEntityTooLarge, "request body too large", err)
			default:
				return wrapRequestError(http.StatusBadRequest, "invalid multipart form",
					wrapBodyError(errorx.Wrap(err, "parse multipart form")))
			}
		}
	}

	lookupFile := func(field reflect.StructField) (reflect.Value, bool, error) {
		var files []*multipart.FileHeader
		if r.MultipartForm != nil {
			files = r.MultipartForm.File[field.Tag.Get("form")]
		}
		switch field.Type {
		case fileHeaderType, fileHeadersType:
			if len(files) == 0 {
				return reflect.Value{}, true, nil
			}
			if field.Type == fileHeaderType {
				return reflect.ValueOf(files[0]), true, nil
			}
			return reflect.ValueOf(slices.Clone(files)), true, nil
		default:
			if len(files) != 0 {
				return reflect.Value{}, true, fmt.Errorf("%w: unsupported file field type %s", mapping.ErrInvalidTarget, field.Type)
			}
			return reflect.Value{}, false, nil
		}
	}
	return wrapBindingError(mapping.BindWith(v, "form", func(name string) []string { return r.Form[name] }, lookupFile))
}

// mediaTypeOf treats a missing Content-Type as absent input.
func mediaTypeOf(r *http.Request) (string, error) {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return "", nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return mediaType, wrapRequestError(http.StatusBadRequest, "invalid Content-Type",
		errorx.Wrap(err, "parse Content-Type"))
}

// multipartBody retains transport errors for typed error classification.
type multipartBody struct {
	io.ReadCloser
	err error
}

func (b *multipartBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}
