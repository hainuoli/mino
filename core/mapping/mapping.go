package mapping

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/hainuoli/mino/core/errorx"
)

// ErrInvalidTarget indicates an invalid bind destination, including
// unsupported field types and inaccessible embedded pointers.
var ErrInvalidTarget = errors.New("mapping: invalid target")

// Bind fills the tagged fields of v with values from lookup.
// v must be a non-nil pointer to a struct. Fields without the tag or without
// input keep their current values; invalid destinations match ErrInvalidTarget.
//
//	type req struct {
//		Page int `query:"page"`
//	}
//
//	var r req
//	err := mapping.Bind(&r, "query", func(name string) []string {
//		return values[name]
//	})
//	// r.Page -> 52 for the query "?page=52"
func Bind(v any, tag string, lookup func(string) []string) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("%w: expected a non-nil pointer to struct, got %T", ErrInvalidTarget, v)
	}
	rv = rv.Elem()
	if lookup == nil {
		return nil
	}
	for _, field := range reflect.VisibleFields(rv.Type()) {
		if !field.IsExported() || hasTaggedParent(rv.Type(), field.Index, tag) {
			continue
		}
		name := field.Tag.Get(tag)
		if name == "" || name == "-" {
			continue
		}
		vs := lookup(name)
		if len(vs) == 0 {
			continue
		}
		fv, err := fieldValue(rv, field.Index)
		if err != nil {
			return errorx.Wrapf(err, "%s %q", tag, name)
		}
		if err := setValue(fv, vs); err != nil {
			return errorx.Wrapf(err, "%s %q", tag, name)
		}
	}
	return nil
}

// hasTaggedParent reports whether any ancestor of the field carries a tag,
// which suppresses the field's promoted binding for that tag.
func hasTaggedParent(t reflect.Type, index []int, tag string) bool {
	for _, i := range index[:len(index)-1] {
		field := t.Field(i)
		if field.Tag.Get(tag) != "" {
			return true
		}
		t = field.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return false
}

// fieldValue returns the field at index, allocating embedded pointers only
// when a value is present. Bind selects only exported fields of an addressable
// struct; the only allocation failure is an unexported embedded pointer.
func fieldValue(v reflect.Value, index []int) (reflect.Value, error) {
	for _, i := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}, fmt.Errorf("%w: field %s is not settable", ErrInvalidTarget, v.Type())
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v, nil
}

// setValue commits pointers and slices after their contents have been parsed.
// TextUnmarshaler takes precedence over the underlying scalar or slice kind.
// Bind and recursive allocation guarantee a settable v and non-empty values.
func setValue(v reflect.Value, values []string) error {
	if v.Kind() == reflect.Pointer {
		next := reflect.New(v.Type().Elem())
		if err := setValue(next.Elem(), values); err != nil {
			return err
		}
		v.Set(next)
		return nil
	}
	if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(values[0]))
	}
	if v.Kind() == reflect.Slice {
		next := reflect.MakeSlice(v.Type(), len(values), len(values))
		for i, value := range values {
			if err := setValue(next.Index(i), []string{value}); err != nil {
				return errorx.Wrapf(err, "index %d", i)
			}
		}
		v.Set(next)
		return nil
	}
	return setScalar(v, values[0])
}

// setScalar parses value according to the kind of v and stores it.
func setScalar(v reflect.Value, value string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(value)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		v.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(value, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		parsed, err := strconv.ParseUint(value, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(parsed)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(value, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(parsed)
	default:
		return fmt.Errorf("%w: unsupported field type %s", ErrInvalidTarget, v.Type())
	}
	return nil
}
