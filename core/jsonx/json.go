package jsonx

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

var (
	errNilCodec  = errors.New("jsonx: nil codec")
	errNilReader = errors.New("jsonx: nil reader")
)

type (
	// Codec defines JSON encoding, decoding, and validation methods.
	// Implementations must be safe for concurrent use.
	Codec interface {
		// Marshal encodes v into a single JSON value without trailing whitespace.
		Marshal(v any) ([]byte, error)

		// Unmarshal decodes a complete JSON document into v.
		// Surrounding whitespace is allowed; extra values and trailing content are not.
		Unmarshal(data []byte, v any) error

		// Valid reports whether data contains a valid JSON document.
		Valid(data []byte) bool
	}

	// mCodec is the default Codec, backed by encoding/json.
	mCodec struct{}
)

var mc Codec = mCodec{}

// SetCodec replaces the process-wide JSON Codec. It affects all calls through
// jsonx, including Mino packages that use jsonx, and is not per-server or
// per-route configuration.
//
// Call it once at startup, before any concurrent JSON operation; later calls
// replace the previous Codec. A nil Codec returns an error and keeps the current one.
//
//	if err := jsonx.SetCodec(sonicCodec); err != nil {
//		panic(err)
//	}
func SetCodec(c Codec) error {
	if isNilCodec(c) {
		return errNilCodec
	}
	mc = c
	return nil
}

func (mCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (mCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func (mCodec) Valid(data []byte) bool {
	return json.Valid(data)
}

// Marshal returns the JSON encoding of v.
//
//	data, err :=jsonx.Marshal(map[string]string{"name": "Hainuo"})
//	// string(data) -> `{"name":"Hainuo"}`
func Marshal(v any) ([]byte, error) {
	return mc.Marshal(v)
}

// MarshalToString returns the JSON encoding of v as a string.
// It returns an empty string when encoding fails.
//
//	data, err := jsonx.MarshalToString(map[string]string{"name": "Hainuo"})
//	// data -> `{"name":"Hainuo"}`
func MarshalToString(v any) (string, error) {
	data, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Unmarshal decodes a complete JSON document into v.
//
//	var user map[string]string
//	err := jsonx.Unmarshal([]byte(`{"name":"Hainuo"}`), &user)
//	// user["name"] -> "Hainuo"
func Unmarshal(data []byte, v any) error {
	return mc.Unmarshal(data, v)
}

// UnmarshalFromString decodes a JSON string into v.
//
//	var user map[string]string
//	err := jsonx.UnmarshalFromString(`{"name":"Hainuo"}`, &user)
//	// user["name"] -> "Hainuo"
func UnmarshalFromString(data string, v any) error {
	return Unmarshal([]byte(data), v)
}

// UnmarshalFromReader reads r to EOF and decodes the content into v.
// It does not close r, Read errors are returned without decoding.
//
//	var user map[string]string
//	r := strings.NewReader(`{"name":"Hainuo"}`)
//	err := jsonx.UnmarshalFromReader(r, &user)
//	// user["name"] -> "Hainuo"
func UnmarshalFromReader(r io.Reader, v any) error {
	if r == nil {
		return errNilReader
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return Unmarshal(data, v)
}

// Valid reports whether data is a complete JSON document.
//
//	if jsonx.Valid(`{"name":"Hainuo"}`)) {
//		// valid JSON
//	}
func Valid(data []byte) bool {
	return mc.Valid(data)
}

// isNilCodec reports whether c is nil, including a typed nil pointer.
func isNilCodec(c Codec) bool {
	if c == nil {
		return true
	}

	// A typed nil is non-nil as an interface value and would panic on its first
	// call, so check the underlying kind too.
	v := reflect.ValueOf(c)

	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func,
		reflect.Chan, reflect.Interface, reflect.UnsafePointer:
		return v.IsNil()
	default:
		return false
	}
}
