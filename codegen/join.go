package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

func (g *gen) joins() {
	for _, j := range g.t.Joins {
		g.join(j)
	}
}

// join emits one declared cross-table read.
//
// The output is a flat row of scalars, so it lives in the DECLARING table's
// package and reuses the whole existing read path: the same token stream, the
// same tree cache, the same binder, the same zero-allocation warm build. A join
// changes what a row carries and which rows qualify; it does not need a
// different machine to run on.
func (g *gen) join(j *schema.Join) {
	cols, err := joinCols(g.t, j)
	if err != nil {
		g.err = err
		return
	}
	low := lowerFirst(j.Name)

	g.p("// %sRow is the %q join: %d column(s) from %d table(s).",
		j.Name, j.Name, len(cols), len(j.Tables)+1)
	g.p("//")
	g.p("// A flat projection, not a graph. Any column taken through a LEFT join is")
	g.p("// nullable whatever its own constraint says — that is what a LEFT join")
	g.p("// means, and typing it otherwise would decode a missing match as a zero.")
	g.p("type %sRow struct {", j.Name)
	for _, c := range cols {
		g.p("\t%s %s", c.field, goType(c.col))
	}
	g.p("}")
	g.p("")

	prefix := pgsql.JoinSelect(g.t.Name, j, func(c schema.CTE) (string, string) {
		return g.cteSQL(c)
	})
	if g.err != nil {
		return
	}
	g.p("const %sPrefix = `%s`", low, prefix)
	g.p("const %sSuffix = `%s`", low, pgsql.JoinSuffix(j))
	if w := pgsql.JoinDeclaredWhere(j); w != "" {
		// The declared predicate is ANDed with whatever the caller adds, so a
		// declaration that says "only fulfilled orders" cannot be widened at a
		// call site. That is the point of declaring it there.
		g.p("const %sWhere = `%s`", low, w)
	}
	g.p("")
	g.p("var (")
	g.p("\t%sCache       = runtime.NewTreeCache()", low)
	g.p("\t%sOffsetCache = runtime.NewTreeCache()", low)
	g.p(")")
	g.p("")
	g.p("func %sStmtFor(toks []runtime.Tok, withOffset bool) *runtime.Stmt {", low)
	g.p("\tc, suffix := %sCache, %sSuffix+limitSuffix", low, low)
	g.p("\tif withOffset {")
	g.p("\t\tc, suffix = %sOffsetCache, %sSuffix+limitOffsetSuffix", low, low)
	g.p("\t}")
	g.p("\tif st := c.Get(toks); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	if pgsql.JoinDeclaredWhere(j) != "" {
		g.p("\treturn c.Put(toks, runtime.SpliceTreeWhere(%sPrefix, %sWhere, toks, lowering, suffix))", low, low)
	} else {
		g.p("\treturn c.Put(toks, runtime.SpliceTree(%sPrefix, toks, lowering, suffix))", low)
	}
	g.p("}")
	g.p("")

	fallible := false
	for _, c := range cols {
		if fallibleIn(c.col, g.dec) {
			fallible = true
		}
	}
	g.p("func scan%s(rv [][]byte, r *%sRow, sl *runtime.Slab) error {", j.Name, j.Name)
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

	g.p("// All%s runs the %q join. Call-site predicates apply to %s and compose",
		j.Name, j.Name, g.t.Name)
	g.p("// with whatever the declaration already filtered.")
	g.p("func (q Query) All%s(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", j.Name, j.Name)
	g.p("\tvar sl runtime.Slab")
	g.p("\treturn q.All%sInto(ctx, ex, nil, &sl)", j.Name)
	g.p("}")
	g.p("")
	g.p("// All%sInto lets the caller own the output slice and the arena.", j.Name)
	g.p("func (q Query) All%sInto(ctx context.Context, ex runtime.Executor, dst []%sRow, sl *runtime.Slab) ([]%sRow, error) {",
		j.Name, j.Name, j.Name)
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	// The declared ORDER BY is part of the statement; a call-site Order would
	// name an unqualified column of a multi-table result.
	g.p("\tif q.no > 0 {")
	g.p("\t\treturn dst, err%sOrdered", j.Name)
	g.p("\t}")
	g.p("\tvar buf [%d]runtime.Tok", g.streamBuf())
	g.p("\tst := %sStmtFor(q.preds(&buf), q.offset > 0)", low)
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
	g.p("\t\tdst = append(dst, %sRow{})", j.Name)
	g.p("\t\tif err := scan%s(rows.RawValues(), &dst[len(dst)-1], sl); err != nil {", j.Name)
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
	g.p("var err%sOrdered = errors.New(", j.Name)
	g.p("\t%q)", "storm: Order() on a join — its ordering is declared, because a column name "+
		"in a multi-table result is ambiguous; sort the returned slice if you need another order")
	g.p("")
}

// cteSQL renders a CTE's body: the declared aggregation, whole.
func (g *gen) cteSQL(c schema.CTE) (string, string) {
	t := g.s.Table(c.Table)
	if t == nil {
		g.err = fmt.Errorf("codegen: CTE %s names table %s, which is not in this context", c.Alias, c.Table)
		return "", ""
	}
	for _, a := range t.Aggregates {
		if a.Name == c.Aggregate {
			return pgsql.AggregateSelect(t.Name, a), pgsql.AggregateSuffix(a)
		}
	}
	g.err = fmt.Errorf("codegen: CTE %s names aggregate %s.%s, which does not exist",
		c.Alias, c.Table, c.Aggregate)
	return "", ""
}

// joinCols resolves the row type.
func joinCols(t *schema.Table, j *schema.Join) ([]aggCol, error) {
	out := make([]aggCol, 0, len(j.Select))
	seen := map[string]bool{}
	for _, c := range j.Select {
		if seen[c.As] {
			return nil, fmt.Errorf("codegen: table %s join %s: two outputs named %s",
				t.Name, j.Name, c.As)
		}
		seen[c.As] = true
		sc := &schema.Column{
			Name:    pgsql.ColumnCase(c.As),
			Type:    c.Type,
			NotNull: !c.Nullable,
		}
		if goKind(sc) == kindUnsupported {
			return nil, fmt.Errorf(
				"codegen: table %s join %s: %s is %s, which has no Go type yet",
				t.Name, j.Name, c.As, c.Type.SQL())
		}
		out = append(out, aggCol{col: sc, field: c.As})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("codegen: table %s join %s selects nothing", t.Name, j.Name)
	}
	return out, nil
}
