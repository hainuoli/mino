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
	// Codec is the JSON implementation used by the package-level functions.
	// Implementations must be safe for concurrent use.
	Codec interface {
		// Marshal returns v encoded as a single JSON value, with no trailing
		// whitespace.
		Marshal(v any) ([]byte, error)

		// Unmarshal decodes a complete JSON document into v. Surrounding
		// whitespace is allowed; extra values and trailing content are not.
		Unmarshal(data []byte, v any) error

		// Valid reports whether data is a complete JSON document in the same
		// syntax Unmarshal accepts.
		Valid(data []byte) bool
	}

	// mCodec is the default Codec, backed by encoding/json.
	mCodec struct{}
)

var mc Codec = mCodec{}

// SetCodec replaces the Codec used by the package-level functions.
//
// Call it once at startup, before any concurrent JSON operation; later calls
// replace the previous Codec. A nil Codec returns an error and keeps the
// current one.
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

// Marshal returns v encoded as a single JSON value using the current Codec.
func Marshal(v any) ([]byte, error) {
	return mc.Marshal(v)
}

// MarshalToString returns v encoded as a JSON string.
// It returns an empty string and the error when encoding fails.
func MarshalToString(v any) (string, error) {
	data, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Unmarshal decodes data into v using the current Codec.
func Unmarshal(data []byte, v any) error {
	return mc.Unmarshal(data, v)
}

// UnmarshalFromString decodes data into v using the current Codec.
func UnmarshalFromString(data string, v any) error {
	return Unmarshal([]byte(data), v)
}

// UnmarshalFromReader reads r to EOF and decodes the content into v.
// It does not close r, and it returns an error without decoding when r is nil
// or reading fails.
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

// Valid reports whether data is a complete JSON document, using the current
// Codec.
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
