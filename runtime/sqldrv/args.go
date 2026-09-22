package sqldrv

import (
	"database/sql/driver"
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
// A TYPE SWITCH, not reflection, and not only because scripts/check has a rule
// about that. The set of types storm BINDS is closed — the generator wrote the
// binder — so enumerating it is both complete and cheaper than asking at run
// time. Anything outside the set passes through untouched and database/sql
// says so, which is the right answer for a type storm did not produce.
//
// The slice is copied only when something actually changes, so a statement
// whose arguments are already driver values allocates nothing.
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
	// The seven database/sql already takes.
	case nil, bool, int64, float64, string, []byte, time.Time:
		return a, false

	// A driver may understand its own type better than a conversion here
	// would, and database/sql calls Value() itself.
	case driver.Valuer:
		return a, false

	// The uuid, and the case that made every insert fail.
	case [16]byte:
		return append([]byte(nil), x[:]...), true

	// Through its decimal STRING, never a float64. The same rule valdec reads
	// by in the other direction: an Oracle NUMBER is exact text on the wire
	// both ways, and a money value through a float64 loses digits the column
	// can hold.
	case runtime.Decimal:
		return x.String(), true
	case runtime.JSON:
		return []byte(x), true

	// The widths a model declares, which database/sql does not take.
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case uint:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		return int64(x), true
	case float32:
		return float64(x), true

	// The pointers a nullable column binds. Dereferencing is ITSELF a change,
	// whatever convert says about the value inside — returning the inner
	// result's flag would leave the pointer in place and bind an address.
	case *bool:
		return derefOrNil(x, func(v bool) any { return v })
	case *int16:
		return derefOrNil(x, func(v int16) any { return int64(v) })
	case *int32:
		return derefOrNil(x, func(v int32) any { return int64(v) })
	case *int64:
		return derefOrNil(x, func(v int64) any { return v })
	case *float32:
		return derefOrNil(x, func(v float32) any { return float64(v) })
	case *float64:
		return derefOrNil(x, func(v float64) any { return v })
	case *string:
		return derefOrNil(x, func(v string) any { return v })
	case *time.Time:
		return derefOrNil(x, func(v time.Time) any { return v })
	}
	return a, false
}

func derefOrNil[T any](p *T, f func(T) any) (any, bool) {
	if p == nil {
		return nil, true
	}
	return f(*p), true
}
