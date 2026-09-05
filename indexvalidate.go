package storm

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Index validation — what a declaration can get wrong that the database would
// report only at apply time, or worse, accept and quietly normalise away so
// that the next diff proposes the same index again.
//
// The second kind is the one worth a build-time rule. PostgreSQL prints only
// the non-default NULL placement back, so an ascending key declared NULLS LAST
// is stored, read back without the flag, compared against the model, and
// dropped and recreated on every run of `storm diff`. That is not a wrong
// index; it is a migration that never converges, which is worse.

// indexMethods are the access methods storm can name, per the target that
// has them.
var indexMethods = map[string]string{
	BTree: "PostgreSQL and MySQL", Hash: "PostgreSQL and MySQL",
	GIN: "PostgreSQL", GiST: "PostgreSQL", SPGiST: "PostgreSQL", BRIN: "PostgreSQL",
	FullText: "MySQL",
}

// opClassMethods lists, for the operator classes storm knows, the methods
// that provide them. An unknown class is passed through for the server to
// judge; a known one on the wrong method is refused here, where the message
// can say which method it wanted.
var opClassMethods = map[string][]string{
	"text_pattern_ops":    {BTree},
	"varchar_pattern_ops": {BTree},
	"bpchar_pattern_ops":  {BTree},
	"jsonb_ops":           {GIN},
	"jsonb_path_ops":      {GIN},
	"array_ops":           {GIN},
	"gin_trgm_ops":        {GIN},
	"gist_trgm_ops":       {GiST},
	"tsvector_ops":        {GIN, GiST},
	"inet_ops":            {GiST, SPGiST},
	"range_ops":           {GiST, SPGiST},
	"point_ops":           {GiST, SPGiST},
	"box_ops":             {GiST},
}

// storageParams lists, per parameter, the methods that accept it.
var storageParams = map[string][]string{
	"fillfactor":             {BTree, Hash, GiST, SPGiST},
	"deduplicate_items":      {BTree},
	"fastupdate":             {GIN},
	"gin_pending_list_limit": {GIN},
	"pages_per_range":        {BRIN},
	"autosummarize":          {BRIN},
	"buffering":              {GiST},
}

// includeMethods are the methods whose leaf entries can carry INCLUDE columns.
var includeMethods = map[string]bool{BTree: true, GiST: true, SPGiST: true}

func (b *builder) validateIndexes() {
	for _, mi := range b.ordered {
		t := mi.tbl.out
		byName := map[string]int{}
		for _, ix := range t.Indexes {
			byName[t.IndexName(ix)]++
		}
		for _, ix := range t.Indexes {
			b.validateIndex(t, ix, byName)
		}
	}
}

func (b *builder) validateIndex(t *schema.Table, ix *schema.Index, byName map[string]int) {
	name := t.IndexName(ix)
	fail := func(format string, a ...any) {
		b.errs.add(fmt.Errorf("%s: index %s "+format, append([]any{t.Name, name}, a...)...))
	}
	if byName[name] > 1 {
		// Two indexes over the same columns — one btree and one hash, or two
		// operator classes — generate the same name and the second CREATE
		// fails on the first's. The definition is legitimate; the name is not.
		fail("would be created twice — two indexes over the same columns need Named(...)")
		return
	}

	method := ix.Method
	if method == "" {
		method = BTree
	}
	if _, ok := indexMethods[method]; !ok {
		known := make([]string, 0, len(indexMethods))
		for m := range indexMethods {
			known = append(known, m)
		}
		sortStrings(known)
		fail("uses access method %q, which storm does not know — one of %s",
			method, strings.Join(known, ", "))
		return
	}
	if ix.Unique && method != BTree {
		fail("is UNIQUE and %s, and only a btree can enforce uniqueness", method)
	}
	if ix.NullsNotDistinct && !ix.Unique {
		fail("is NULLS NOT DISTINCT but not UNIQUE — the clause only means something for a uniqueness check")
	}
	if method == Hash && len(ix.Columns) > 1 {
		fail("is a hash over %d columns; a hash index has exactly one key", len(ix.Columns))
	}
	if len(ix.Include) > 0 && !includeMethods[method] {
		fail("carries INCLUDE columns on a %s index; only btree, gist and spgist leaf entries can carry them", method)
	}
	for _, p := range ix.With {
		methods, ok := storageParams[p.Name]
		if !ok {
			known := make([]string, 0, len(storageParams))
			for k := range storageParams {
				known = append(known, k)
			}
			sortStrings(known)
			fail("sets storage parameter %q, which storm does not know — one of %s", p.Name, strings.Join(known, ", "))
			continue
		}
		if !containsStr(methods, method) {
			fail("sets %s, a %s parameter, on a %s index", p.Name, strings.Join(methods, "/"), method)
		}
	}

	keys := map[string]bool{}
	for _, c := range ix.Columns {
		if !c.Expr {
			if keys[c.Name] {
				fail("names column %s twice", c.Name)
			}
			keys[c.Name] = true
		}
		b.validateKey(t, ix, method, c, fail)
	}
	for _, inc := range ix.Include {
		if keys[inc] {
			fail("has %s as both a key and an INCLUDE column; a key is already in every entry", inc)
		}
	}
	if method == FullText {
		for _, c := range ix.Columns {
			if c.Expr || !textual(t.Column(c.Name)) {
				fail("is FULLTEXT over %s, which is not a text column", c.Name)
			}
		}
	}
}

func (b *builder) validateKey(t *schema.Table, ix *schema.Index, method string,
	c schema.IndexColumn, fail func(string, ...any)) {
	switch {
	case c.NullsFirst && c.NullsLast:
		fail("orders %s NULLS FIRST and NULLS LAST", c.Name)
	case c.Desc && c.NullsFirst:
		fail("orders %s DESC NULLS FIRST, which is where a descending key already puts NULLs — "+
			"the database will not remember the clause, and every diff would recreate the index", c.Name)
	case !c.Desc && c.NullsLast:
		fail("orders %s ASC NULLS LAST, which is where an ascending key already puts NULLs — "+
			"the database will not remember the clause, and every diff would recreate the index", c.Name)
	}
	if c.OpClass != "" {
		if methods, known := opClassMethods[c.OpClass]; known && !containsStr(methods, method) {
			fail("gives %s the operator class %s, which belongs to %s, on a %s index — Using(storm.%s)",
				c.Name, c.OpClass, strings.Join(methods, "/"), method, constName(methods[0]))
		}
	}
	if c.Prefix > 0 {
		if c.Expr {
			fail("indexes a %d-character prefix of an expression; a prefix applies to a column", c.Prefix)
		} else if !textual(t.Column(c.Name)) && !binary(t.Column(c.Name)) {
			fail("indexes a %d-character prefix of %s, which is not a text or bytes column", c.Prefix, c.Name)
		}
	}
	_ = ix
}

func textual(c *schema.Column) bool {
	if c == nil {
		return false
	}
	return c.Type.Name == schema.TypeText || c.Type.Name == schema.TypeVarchar
}

func binary(c *schema.Column) bool {
	return c != nil && c.Type.Name == schema.TypeBytea
}

// constName is the Go constant for a method, for an error's fix.
func constName(method string) string {
	switch method {
	case BTree:
		return "BTree"
	case GIN:
		return "GIN"
	case GiST:
		return "GiST"
	case SPGiST:
		return "SPGiST"
	case Hash:
		return "Hash"
	case BRIN:
		return "BRIN"
	case FullText:
		return "FullText"
	}
	return method
}

func containsStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
