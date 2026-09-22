package sqldrv

import (
	"database/sql/driver"
	"reflect"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// normalize converts storm's bound values into the types database/sql accepts.
//
// THIS IS NOT OPTIONAL PLUMBING. database/sql takes exactly seven kinds —
// nil, bool, int64, float64, string, []byte and time.Time — unless a driver
// registers a converter, and storm's generated binders produce richer ones: a
// uuid is a [16]byte, a decimal is runtime.Decimal, an int32 column binds an
// int32. Passing those through unconverted is a driver error per call.
//
// AND ONE OF THEM IS WORSE THAN AN ERROR. go-ora reads an ARRAY-typed argument
// as a request for array binding and answers any insert carrying one with "to
// activate bulk insert/merge all parameters should be arrays" — so a [16]byte
// key, which every storm model has, made every insert fail. Found by running
// the generated package; nothing before that had bound a uuid through this
// adapter.
//
// The slice is copied only when something actually changes, so the common case
// — a statement whose arguments are already driver values — allocates nothing.
func normalize(args []any) []any {
	out := args
	copied := false
	for i, a := range args {
		v, changed := convert(a)
		if !changed {
			continue
		}
		if !copied {
			out = append([]any(nil), args...)
			copied = true
		}
		out[i] = v
	}
	return out
}

func convert(a any) (any, bool) {
	switch x := a.(type) {
	case nil, bool, int64, float64, string, []byte, time.Time:
		return a, false
	case driver.Valuer:
		// database/sql calls this itself, and a driver may understand the
		// concrete type better than a conversion here would.
		return a, false
	case runtime.Decimal:
		// Through its DECIMAL STRING, never a float64. The same rule valdec
		// reads by, in the other direction: an Oracle NUMBER is exact text on
		// the wire both ways, and routing a money value through a float64
		// would lose digits the column can hold.
		return x.String(), true
	case runtime.JSON:
		return []byte(x), true
	}

	rv := reflect.ValueOf(a)
	switch rv.Kind() {
	case reflect.Array:
		// A [16]byte uuid, and the case that made every insert fail. Also the
		// one database/sql would have refused anyway.
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, rv.Len())
			reflect.Copy(reflect.ValueOf(b), rv)
			return b, true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()), true
	case reflect.Float32:
		return rv.Float(), true
	case reflect.Ptr:
		// Dereferencing is ITSELF a change, whatever convert says about the
		// value inside — returning the inner result's flag left the pointer in
		// place and bound an address.
		if rv.IsNil() {
			return nil, true
		}
		inner, _ := convert(rv.Elem().Interface())
		return inner, true
	case reflect.Slice:
		// A named []byte — runtime.JSON is handled above, but a model may
		// declare its own.
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Bytes(), true
		}
	}
	return a, false
}
