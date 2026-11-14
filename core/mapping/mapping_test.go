package mapping

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lookup returns a Bind lookup function over the given values.
func lookup(values map[string][]string) func(string) []string {
	return func(key string) []string { return values[key] }
}

func ExampleBind() {
	var config struct {
		Host string `config:"host"`
		Port int    `config:"port"`
	}
	values := map[string][]string{"host": {"localhost"}, "port": {"8080"}}
	if err := Bind(&config, "config", func(key string) []string {
		return values[key]
	}); err != nil {
		panic(err)
	}
	fmt.Println(config.Host, config.Port)
	// Output: localhost 8080
}

// Options is an embedded struct, whose pointer is allocated only on demand.
type Options struct {
	Limit *int `value:"limit"`
}

// textID parses only the name "admin", so bind errors carry a distinct cause.
type textID int

var errUnknownID = errors.New("unknown ID")

func (id *textID) UnmarshalText(data []byte) error {
	if string(data) != "admin" {
		return errUnknownID
	}
	*id = 42
	return nil
}

// words splits its text on commas, exercising TextUnmarshaler over slices.
type words []string

func (w *words) UnmarshalText(data []byte) error {
	*w = strings.Split(string(data), ",")
	return nil
}

func TestBindTypes(t *testing.T) {
	var dst struct {
		*Options
		Name string    `value:"name"`
		Flag bool      `value:"flag"`
		N    int8      `value:"n"`
		U    uint16    `value:"u"`
		F    float32   `value:"f"`
		IDs  []*int    `value:"id"`
		When time.Time `value:"when"`
		Role textID    `value:"role"`
		Tags words     `value:"tags"`
	}
	require.NoError(t, Bind(&dst, "value", nil))
	assert.Nil(t, dst.Options)
	values := map[string][]string{
		"limit": {"0"}, "name": {"first", "second"}, "flag": {"true"},
		"n": {"-2"}, "u": {"9"}, "f": {"1.5"}, "id": {"3", "4"},
		"when": {"2026-01-01T00:00:00Z"}, "role": {"admin"}, "tags": {"a,b", "ignored"},
	}
	require.NoError(t, Bind(&dst, "value", lookup(values)))
	require.NotNil(t, dst.Options)
	require.NotNil(t, dst.Limit)
	assert.Zero(t, *dst.Limit)
	assert.Equal(t, "first", dst.Name)
	assert.True(t, dst.Flag)
	assert.Equal(t, int8(-2), dst.N)
	assert.Equal(t, uint16(9), dst.U)
	assert.Equal(t, float32(1.5), dst.F)
	require.Len(t, dst.IDs, 2)
	assert.Equal(t, 3, *dst.IDs[0])
	assert.Equal(t, 4, *dst.IDs[1])
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), dst.When)
	assert.Equal(t, textID(42), dst.Role)
	assert.Equal(t, words{"a", "b"}, dst.Tags)
}

func TestBindPresence(t *testing.T) {
	dst := &struct {
		Keep     string `value:"missing"`
		Empty    string `value:"empty"`
		Ignored  string `value:"-"`
		Untagged string
		private  string `value:"private"`
	}{
		Keep:     "default",
		Empty:    "default",
		Ignored:  "default",
		Untagged: "default",
		private:  "default",
	}
	values := map[string][]string{"empty": {""}, "-": {"input"}, "Untagged": {"input"}, "private": {"input"}}
	require.NoError(t, Bind(dst, "value", lookup(values)))
	assert.Empty(t, dst.Empty)
	assert.Equal(t, "default", dst.Keep)
	assert.Equal(t, "default", dst.Ignored)
	assert.Equal(t, "default", dst.Untagged)
	assert.Equal(t, "default", dst.private)

	// Binding another source preserves values not supplied by that source.
	require.NoError(t, Bind(dst, "value", lookup(map[string][]string{"missing": {"next"}})))
	assert.Equal(t, "next", dst.Keep)
	assert.Empty(t, dst.Empty)
}

func TestBindInputErrors(t *testing.T) {
	tests := []struct {
		name  string
		dst   any
		value string
	}{
		{"bool", &struct {
			N bool `value:"n"`
		}{}, "yes"},
		{"int overflow", &struct {
			N int8 `value:"n"`
		}{}, "128"},
		{"uint", &struct {
			N uint `value:"n"`
		}{}, "-1"},
		{"float overflow", &struct {
			N float32 `value:"n"`
		}{}, "1e100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Bind(test.dst, "value", lookup(map[string][]string{"n": {test.value}}))
			require.Error(t, err)
			var numErr *strconv.NumError
			assert.ErrorAs(t, err, &numErr)
			assert.NotErrorIs(t, err, ErrInvalidTarget)
			assert.Contains(t, err.Error(), `value "n"`)
		})
	}
	var dst struct {
		Role textID `value:"role"`
	}
	err := Bind(&dst, "value", lookup(map[string][]string{"role": {"unknown"}}))
	assert.ErrorIs(t, err, errUnknownID)
	assert.NotErrorIs(t, err, ErrInvalidTarget)
}

func TestBindFailedPointerAndSlice(t *testing.T) {
	original := 7
	dst := struct {
		N   *int  `value:"n"`
		IDs []int `value:"id"`
	}{N: &original, IDs: []int{1, 2}}
	err := Bind(&dst, "value", lookup(map[string][]string{"n": {"bad"}}))
	require.Error(t, err)
	assert.Same(t, &original, dst.N)
	err = Bind(&dst, "value", lookup(map[string][]string{"id": {"3", "bad"}}))
	require.Error(t, err)
	assert.Equal(t, []int{1, 2}, dst.IDs)
	assert.Contains(t, err.Error(), "index 1")
}

type privateOptions struct {
	Count int `value:"count"`
}

func TestBindInvalidTargets(t *testing.T) {
	tests := []struct {
		name string
		dst  any
	}{
		{"nil", nil},
		{"non-pointer struct", struct{}{}},
		{"nil struct pointer", (*struct{})(nil)},
		{"non-struct pointer", new(int)},
		{"unsupported field type", &struct {
			N map[string]int `value:"n"`
		}{}},
		{"unexported embedded pointer", &struct{ *privateOptions }{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Bind(test.dst, "value", lookup(map[string][]string{"n": {"1"}, "count": {"1"}}))
			assert.ErrorIs(t, err, ErrInvalidTarget)
		})
	}
}

func TestBindEmbeddedFields(t *testing.T) {
	values := lookup(map[string][]string{"limit": {"3"}, "count": {"4"}})
	var lazy struct{ *Options }
	require.NoError(t, Bind(&lazy, "value", lookup(nil)))
	assert.Nil(t, lazy.Options)
	require.NoError(t, Bind(&lazy, "value", values))
	require.NotNil(t, lazy.Options)
	assert.Equal(t, 3, *lazy.Limit)

	var ignored struct {
		Options `value:"-"`
	}
	require.NoError(t, Bind(&ignored, "value", values))
	assert.Nil(t, ignored.Limit)

	// Exported children of an unexported embedded value remain writable.
	var embedded struct{ privateOptions }
	require.NoError(t, Bind(&embedded, "value", values))
	assert.Equal(t, 4, embedded.Count)

	initialized := struct{ *privateOptions }{privateOptions: &privateOptions{}}
	require.NoError(t, Bind(&initialized, "value", values))
	assert.Equal(t, 4, initialized.Count)

	var shadowed struct {
		Options
		Limit int `value:"limit"`
	}
	require.NoError(t, Bind(&shadowed, "value", values))
	assert.Equal(t, 3, shadowed.Limit)
	assert.Nil(t, shadowed.Options.Limit)
}
