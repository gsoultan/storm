package spike

import (
	"strconv"
	"sync/atomic"
)

// One bit per optional predicate. Six filters => 64 shapes.
const (
	fOrg uint32 = 1 << iota
	fEmail
	fName
	fAge
	fStatus
	fSince

	nFilters = 6
	nShapes  = 1 << nFilters
)

const selectPrefix = `SELECT id, org_id, email, name, age, status, created_at, updated_at FROM users`
const orderSuffix = ` ORDER BY created_at DESC, id LIMIT $`

// frag is a predicate lowered at build time. It renders as a + <ordinal> + b,
// so the placeholder number is the only thing decided per shape.
type frag struct{ a, b string }

var frags = [nFilters]frag{
	{a: `org_id = $`},
	{a: `email = $`},
	{a: `name LIKE $`},
	{a: `age >= $`},
	{a: `status = $`},
	{a: `created_at >= $`},
}

// stmt is a compiled statement: immutable, interned, shared by every caller
// that produces the same shape.
type stmt struct {
	sql  string
	nArg int
	// hint is the byte size the last result of this shape needed from a Slab.
	// Shapes are stable, so one observation sizes the next arena exactly and
	// the doubling ramp never runs.
	hint atomic.Int64
}

// cache is an indexed array, not a map — the mask is already a perfect hash,
// so there is nothing to hash on the warm path.
var cache [nShapes]atomic.Pointer[stmt]

// lookup returns the compiled statement for a shape, compiling it at most once
// per process. A lost CAS race just discards the loser's copy.
func lookup(mask uint32) *stmt {
	if s := cache[mask].Load(); s != nil {
		return s
	}
	s := compile(mask)
	cache[mask].CompareAndSwap(nil, s)
	return cache[mask].Load()
}

func compile(mask uint32) *stmt {
	b := make([]byte, 0, 256)
	b = append(b, selectPrefix...)

	ord := 0
	for i := 0; i < nFilters; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		if ord == 0 {
			b = append(b, " WHERE "...)
		} else {
			b = append(b, " AND "...)
		}
		ord++
		b = append(b, frags[i].a...)
		b = strconv.AppendInt(b, int64(ord), 10)
		b = append(b, frags[i].b...)
	}

	b = append(b, orderSuffix...)
	ord++
	b = strconv.AppendInt(b, int64(ord), 10)

	return &stmt{sql: string(b), nArg: ord}
}

// ResetCacheForTest clears the compiled-statement cache.
func ResetCacheForTest() {
	for i := range cache {
		cache[i].Store(nil)
	}
}

// CompileForTest renders a shape without consulting the cache, so cold-path
// benchmarks measure compilation itself rather than timer juggling.
func CompileForTest(mask uint32) string { return compile(mask).sql }

// NumShapes is the number of distinct shapes this table can produce.
const NumShapes = nShapes
