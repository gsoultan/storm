package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/compile/pgsql"
)

// Named projection emission.
//
// A projection shares EVERYTHING with the full read except the SELECT list and
// the row: same predicates, same ordering, same keyset tokens, same caches
// pattern. So it is emitted as: a row type, a scanner, a prefix constant, two
// statement caches, and two terminals — and nothing else, because everything
// else already exists.

func (g *gen) projections() {
	for _, pr := range g.t.Projections {
		g.projection(pr.Name, pr.Columns)
	}
}

func (g *gen) projection(name string, columns []string) {
	// Resolve to colInfo in DECLARATION order — it becomes the field order.
	var cols []colInfo
	for _, cn := range columns {
		c := g.t.Column(cn)
		if c == nil {
			g.err = fmt.Errorf("codegen: table %s projection %s: no column %s", g.t.Name, name, cn)
			return
		}
		k := goKind(c)
		if k == kindUnsupported {
			g.err = fmt.Errorf(
				"codegen: table %s projection %s: column %s (%s) has no Go type yet",
				g.t.Name, name, cn, c.Type.SQL())
			return
		}
		cols = append(cols, colInfo{col: c, kind: k, goBase: baseGoType(c)})
	}

	low := lowerFirst(name)
	g.p("// %sRow is the %q projection: the same read, %d column(s) instead of", name, name, len(cols))
	g.p("// the whole row. Narrower tuples, no TOAST fetch for what nobody asked,")
	g.p("// and an index-only scan becomes POSSIBLE — the full-row read forecloses")
	g.p("// it by construction.")
	g.p("type %sRow struct {", name)
	for _, c := range cols {
		g.p("\t%s %s", exportName(c.Name()), goType(c.col))
	}
	g.p("}")
	g.p("")

	g.p("const %sPrefix = `%s`", low, pgsql.SelectPrefix(g.t.Name, columns))
	g.p("")
	g.p("var (")
	g.p("\t%sCache       = runtime.NewTreeCache()", low)
	g.p("\t%sOffsetCache = runtime.NewTreeCache()", low)
	g.p(")")
	g.p("")
	g.p("func %sStmtFor(toks []runtime.Tok, withOffset bool) *runtime.Stmt {", low)
	g.p("\tc, suffix := %sCache, limitSuffix", low)
	g.p("\tif withOffset {")
	g.p("\t\tc, suffix = %sOffsetCache, limitOffsetSuffix", low)
	g.p("\t}")
	g.p("\tif st := c.Get(toks); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\treturn c.Put(toks, runtime.SpliceTree(%sPrefix, toks, lowering, suffix))", low)
	g.p("}")
	g.p("")

	fallible := false
	for _, c := range cols {
		if fallibleIn(c.col, g.dec) {
			fallible = true
		}
	}
	g.p("func scan%s(rv [][]byte, r *%sRow, sl *runtime.Slab) error {", name, name)
	if fallible {
		g.p("\tvar decErr error")
	}
	for i, c := range cols {
		g.p("\t%s", decodeExprIn(c.col, i, g.dec))
		if fallibleIn(c.col, g.dec) {
			g.p("\tif decErr != nil {")
			g.p("\t\treturn decErr")
			g.p("\t}")
		}
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")

	g.p("// All%s runs the query projected to %sRow. Predicates, ordering, limit,", name, name)
	g.p("// offset and keyset all apply exactly as on All — a projection changes")
	g.p("// what a row CARRIES, never which rows qualify. (After takes the full Row;")
	g.p("// populate its ordering columns and the cursor works unchanged.)")
	g.p("func (q Query) All%s(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", name, name)
	g.p("\tvar sl runtime.Slab")
	g.p("\treturn q.All%sInto(ctx, ex, nil, &sl)", name)
	g.p("}")
	g.p("")
	g.p("// All%sInto lets the caller own the output slice and the arena, exactly", name)
	g.p("// as AllInto does — a projection's terminals mirror the full read's, so a")
	g.p("// hot loop reuses both and a benchmark compares like with like.")
	g.p("func (q Query) All%sInto(ctx context.Context, ex runtime.Executor, dst []%sRow, sl *runtime.Slab) ([]%sRow, error) {", name, name, name)
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tvar buf [%d]runtime.Tok", maxToks+maxOrder+1)
	g.p("\tst := %sStmtFor(q.stream(&buf), q.offset > 0)", low)
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn dst, st.Err")
	g.p("\t}")
	g.p("\tsl.Reserve(st.SlabHint())")
	g.p("\tb := binders.Get()")
	g.p("\tdefer putBinder(b)")
	g.p("\trows, err := ex.Query(ctx, st.SQL, q.bind(b))")
	g.p("\tif err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tfor rows.Next() {")
	g.p("\t\tdst = append(dst, %sRow{})", name)
	g.p("\t\tif err := scan%s(rows.RawValues(), &dst[len(dst)-1], sl); err != nil {", name)
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")

	g.p("// One%s is All%s stopped at one row.", name, name)
	g.p("func (q Query) One%s(ctx context.Context, ex runtime.Executor) (%sRow, bool, error) {", name, name)
	g.p("\tout, err := q.Limit(1).All%s(ctx, ex)", name)
	g.p("\tif err != nil || len(out) == 0 {")
	g.p("\t\treturn %sRow{}, false, err", name)
	g.p("\t}")
	g.p("\treturn out[0], true, nil")
	g.p("}")
	g.p("")
}
