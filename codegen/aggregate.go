package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// aggregate emits one declared aggregation: its row type, its scanner, and the
// terminal on Query that runs it.
//
// The predicates stay dynamic — the same token stream, the same tree cache, the
// same zero-allocation warm path as every other read. What is fixed at
// generation time is the SELECT list and the GROUP BY, which is exactly the
// part that cannot be enumerated if it is composed at a call site.
func (g *gen) aggregate(agg *schema.Aggregate) {
	name := agg.Name
	cols, err := aggregateCols(g.t, agg)
	if err != nil {
		g.err = err
		return
	}

	low := lowerFirst(name)
	g.p("// %sRow is the %q aggregation over %s.", name, name, g.t.Name)
	if len(agg.By) > 0 {
		g.p("//")
		g.p("// One row per group. The grouping columns come first, in declaration")
		g.p("// order, and the result is ordered by them: PostgreSQL promises no")
		g.p("// order for a GROUP BY, and an unordered report shuffles between")
		g.p("// requests for no reason anyone can see.")
	} else {
		g.p("//")
		g.p("// No grouping columns: the whole table is one group, so this is")
		g.p("// exactly one row however many rows it read.")
	}
	g.p("type %sRow struct {", name)
	for _, c := range cols {
		g.p("\t%s %s", c.field, goType(c.col))
	}
	g.p("}")
	g.p("")

	g.p("const %sPrefix = `%s`", low, pgsql.AggregateSelect(g.t.Name, agg))
	// GROUP BY and ORDER BY sit between the predicates and the paging, which
	// is where SQL wants them and where the splice puts them.
	g.p("const %sSuffix = `%s`", low, pgsql.AggregateSuffix(agg))
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

	g.p("// All%s runs the %q aggregation. Predicates compose exactly as on All —", name, name)
	g.p("// they filter the rows that go INTO the groups, which is the WHERE clause")
	g.p("// and not a HAVING. Limit and offset page the groups.")
	g.p("func (q Query) All%s(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", name, name)
	g.p("\tvar sl runtime.Slab")
	g.p("\treturn q.All%sInto(ctx, ex, nil, &sl)", name)
	g.p("}")
	g.p("")
	g.p("// All%sInto lets the caller own the output slice and the arena.", name)
	g.p("func (q Query) All%sInto(ctx context.Context, ex runtime.Executor, dst []%sRow, sl *runtime.Slab) ([]%sRow, error) {", name, name, name)
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	// An ordering set on the base query cannot survive: the SELECT list is the
	// grouping columns and the aggregates, so ORDER BY on an ungrouped column
	// is an error PostgreSQL raises and storm can refuse first.
	g.p("\tif q.no > 0 {")
	g.p("\t\treturn dst, err%sOrdered", name)
	g.p("\t}")
	g.p("\tvar buf [%d]runtime.Tok", maxToks+maxOrder+1)
	// preds, NOT stream: stream appends the query's DEFAULT ordering, and a
	// grouped read may only order by its grouping columns. Splicing the
	// default in produced `... ORDER BY "id" GROUP BY "status" ...` — a syntax
	// error — or, once past that, PostgreSQL's "column orders.id must appear
	// in the GROUP BY clause". The ordering this statement needs is in its own
	// suffix.
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
	g.p("\t\tdst = append(dst, %sRow{})", name)
	g.p("\t\tif err := scan%s(rows.RawValues(), &dst[len(dst)-1], sl); err != nil {", name)
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
	g.p("var err%sOrdered = errors.New(", name)
	g.p("\t%q)", "storm: Order() on an aggregation — its rows are groups, not table rows, "+
		"and "+g.t.Name+"."+name+" is already ordered by its grouping columns; "+
		"sort the returned slice if you need another order")
	g.p("")

	if len(agg.By) == 0 {
		g.p("// One%s is the whole-table aggregate: exactly one row, always.", name)
		g.p("func (q Query) One%s(ctx context.Context, ex runtime.Executor) (%sRow, error) {", name, name)
		g.p("\tout, err := q.All%s(ctx, ex)", name)
		g.p("\tif err != nil || len(out) == 0 {")
		g.p("\t\treturn %sRow{}, err", name)
		g.p("\t}")
		g.p("\treturn out[0], nil")
		g.p("}")
		g.p("")
	}
}

// aggCol is one output field: the synthetic column it decodes as, and the Go
// field name it lands in.
type aggCol struct {
	col   *schema.Column
	field string
}

// aggregateCols resolves the row type.
//
// Grouping expressions keep their own type — a NULL groups with other NULLs
// rather than disappearing. Aggregate and window terms carry the type the
// expression resolved to, which for an aggregate is usually not its input type.
func aggregateCols(t *schema.Table, agg *schema.Aggregate) ([]aggCol, error) {
	var out []aggCol
	seen := map[string]string{}

	claim := func(field, from string) error {
		if prev, dup := seen[field]; dup {
			return fmt.Errorf(
				"codegen: table %s aggregate %s: %s and %s both produce the field %s — rename one",
				t.Name, agg.Name, prev, from, field)
		}
		seen[field] = from
		return nil
	}

	for _, g := range agg.By {
		if err := claim(g.As, "grouping "+g.As); err != nil {
			return nil, err
		}
		nullable := g.Expr.Nullable
		// A grouping SET makes every grouping column nullable regardless of
		// the column's own constraint: a subtotal row carries NULL for the
		// columns it aggregated over. Typing these as non-null would decode a
		// ROLLUP's subtotal as a zero value — the empty string for a status,
		// the epoch for a day — and the report would be quietly wrong rather
		// than loudly broken.
		if agg.Sets != nil {
			nullable = true
		}
		sc := &schema.Column{
			Name:    pgsqlAlias(g.As),
			Type:    g.Expr.Type,
			NotNull: !nullable,
		}
		if goKind(sc) == kindUnsupported {
			return nil, fmt.Errorf(
				"codegen: table %s aggregate %s: grouping %s is %s, which has no Go type yet",
				t.Name, agg.Name, g.As, g.Expr.Type.SQL())
		}
		out = append(out, aggCol{col: sc, field: g.As})
	}

	for _, term := range agg.Terms {
		if err := claim(term.As, describe(term.Expr)); err != nil {
			return nil, err
		}
		// A synthetic column, so the row type and the decoder come from the
		// same code paths every other read uses. NotNull inverted from the
		// expression's resolved nullability is where "no rows sums to NULL"
		// becomes a runtime.Null[T] rather than a silent zero.
		sc := &schema.Column{
			Name:    pgsqlAlias(term.As),
			Type:    term.Expr.Type,
			NotNull: !term.Expr.Nullable,
		}
		if goKind(sc) == kindUnsupported {
			return nil, fmt.Errorf(
				"codegen: table %s aggregate %s: %s returns %s, which has no Go type yet",
				t.Name, agg.Name, term.As, term.Expr.Type.SQL())
		}
		out = append(out, aggCol{col: sc, field: term.As})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("codegen: table %s aggregate %s selects nothing", t.Name, agg.Name)
	}
	return out, nil
}

// describe names an expression for an error message.
func describe(e schema.Expr) string {
	switch e.Kind {
	case schema.ExprAgg, schema.ExprFunc, schema.ExprWindow:
		return e.Fn + "()"
	case schema.ExprGrouping:
		return "GROUPING()"
	case schema.ExprCol:
		return "column " + e.Col
	}
	return "expression"
}

// pgsqlAlias mirrors the alias the SQL back end derives from a field name. The
// column is synthetic, so this name only ever appears in error messages — but
// an error naming a column nobody can find is worse than no error.
func pgsqlAlias(field string) string { return pgsql.ColumnCase(field) }

func (g *gen) aggregates() {
	for _, agg := range g.t.Aggregates {
		g.aggregate(agg)
	}
}
