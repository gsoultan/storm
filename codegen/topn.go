package codegen

import (
	"fmt"
	"strings"

	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Greatest-n-per-group emission: "each parent with its first N children",
// in one query rather than one per parent.
//
// Generated into the CHILD's package, because that is where the child's columns
// and orderings are. The plan layer in the context package calls it.

// TopStrategy selects the lowering.
type TopStrategy int

const (
	// TopDefault is the zero value: whatever DefaultTopStrategy measures as
	// fastest. Options left unset get the measured default rather than
	// whichever constant happened to be declared first.
	TopDefault TopStrategy = iota
	// TopWindow numbers rows within each parent and discards those past N.
	TopWindow
	// TopLateral runs a limited, ordered scan per parent key.
	TopLateral
)

// DefaultTopStrategy is chosen by measurement, not by argument. See
// bench/RESULTS.md; change it there and here together or the comment is a lie.
//
// Measured 2026-08-24: LATERAL wins at every parent count and every n, and the
// gap widens with parent count — 3.5x at one parent, 13x at ten, 33x at a
// hundred. The first cut of this file defaulted to the window form on the
// reasoning that one index scan feeding a window beats a per-parent nested
// loop. That reasoning was wrong: the window form reads EVERY child of every
// matched parent and discards the ones past n, so its cost tracks the total
// child count while LATERAL's tracks the rows actually returned.
const DefaultTopStrategy = TopLateral

// batchTop emits one per-parent limited loader per partition column.
func (g *gen) batchTop() {
	if len(g.o.BatchTopColumns) == 0 {
		return
	}
	cols := readableCols(g.t)

	g.p("// Greatest-n-per-group: at most n children per parent, in ONE query.")
	g.p("//")
	g.p("// The ordering is REQUIRED and must be a strict total order. \"The first")
	g.p("// three by date\" with ties on that date returns an arbitrary three, and")
	g.p("// a different arbitrary three on the next call — a paging bug that only")
	g.p("// appears under data the developer did not have. Add the primary key as")
	g.p("// a final term if the natural ordering is not unique.")
	g.p("var errNoTopOrder = errors.New(")
	g.p("\t%q)", "raorm: a per-parent limit needs an ordering — without one "+
		"\"the first three\" is an arbitrary three, and a different three next time")
	g.p("")

	for _, key := range g.o.BatchTopColumns {
		kc := g.t.Column(key)
		if kc == nil {
			g.err = fmt.Errorf("codegen: table %s has no column %s to batch by", g.t.Name, key)
			return
		}
		ki := g.colIndex(key)
		if ki < 0 {
			g.err = fmt.Errorf(
				"codegen: table %s column %s is not filterable, so it cannot partition a per-parent load",
				g.t.Name, key)
			return
		}
		g.topFn(key, kc, cols)
	}
}

func (g *gen) topFn(key string, kc *schema.Column, cols []string) {
	name := "BatchTopBy" + exportName(key)
	priv := lowerFirst(name)
	keyGo := baseGoType(kc)

	strategy := g.o.TopStrategy
	if strategy == TopDefault {
		strategy = DefaultTopStrategy
	}
	def := "Window"
	if strategy == TopLateral {
		def = "Lateral"
	}

	g.p("// %s fetches at most n rows for each id, ordered by order.", name)
	g.p("//")
	g.p("// ONE round trip regardless of how many ids are passed — the per-parent")
	g.p("// limit is expressed in SQL, not by looping. It delegates to the lowering")
	g.p("// chosen at BUILD time by measurement (bench/RESULTS.md); the strategy is")
	g.p("// never sniffed at run time.")
	g.p("func %s(ctx context.Context, ex runtime.Executor, ids []%s, n int64, order ...Sort) ([]Row, error) {", name, keyGo)
	g.p("\treturn %s%s(ctx, ex, ids, n, order...)", name, def)
	g.p("}")
	g.p("")

	for _, form := range []string{"Window", "Lateral"} {
		g.p("// %s%s runs the %s lowering.", name, form, strings.ToLower(form))
		g.p("//")
		g.p("// Both forms are exported so a benchmark can compare them on identical")
		g.p("// generated code rather than on two hand-written approximations, and so")
		g.p("// a caller whose data defeats the default has a way out that does not")
		g.p("// involve writing SQL.")
		g.p("func %s%s(ctx context.Context, ex runtime.Executor, ids []%s, n int64, order ...Sort) ([]Row, error) {", name, form, keyGo)
		g.p("\treturn %sRun(ctx, ex, %s%sSQL(order), ids, n)", priv, priv, form)
		g.p("}")
		g.p("")
	}

	g.p("// %sRun executes one lowered statement.", priv)
	g.p("func %sRun(ctx context.Context, ex runtime.Executor, sql string, ids []%s, n int64) ([]Row, error) {", priv, keyGo)
	g.p("\tif len(ids) == 0 || n <= 0 {")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\tif sql == \"\" {")
	g.p("\t\treturn nil, errNoTopOrder")
	g.p("\t}")
	g.p("\trows, err := ex.Query(ctx, sql, []any{ids, n})")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tvar sl runtime.Slab")
	g.p("\tout := make([]Row, 0, int64(len(ids))*n)")
	g.p("\tfor rows.Next() {")
	g.p("\t\tout = append(out, Row{})")
	g.p("\t\tif err := scan(rows.RawValues(), &out[len(out)-1], &sl); err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn out, rows.Err()")
	g.p("}")
	g.p("")

	for _, form := range []string{"Window", "Lateral"} {
		g.p("var %s%sCache = runtime.NewTreeCache()", priv, form)
		g.p("")
		g.p("// %s%sSQL compiles one ordering, once. The ordering is the key, exactly", priv, form)
		g.p("// as it is for an ordinary read; an empty ordering yields \"\", which the")
		g.p("// runner turns into errNoTopOrder.")
		g.p("func %s%sSQL(order []Sort) string {", priv, form)
		g.p("\tif len(order) == 0 {")
		g.p("\t\treturn \"\"")
		g.p("\t}")
		g.p("\ttoks := make([]runtime.Tok, len(order))")
		g.p("\tfor i, s := range order {")
		g.p("\t\ttoks[i] = runtime.Tok(s)")
		g.p("\t}")
		g.p("\tif st := %s%sCache.Get(toks); st != nil {", priv, form)
		g.p("\t\treturn st.SQL")
		g.p("\t}")
		g.p("\tterms := make([]string, len(toks))")
		g.p("\tfor i, t := range toks {")
		g.p("\t\tterms[i] = orderOf(t.Op(), t.Col())")
		g.p("\t}")
		var unordered string
		if form == "Window" {
			unordered = pgsql.TopNWindow(g.t.Name, cols, key)
		} else {
			unordered = pgsql.TopNLateral(g.t.Name, cols, key, kc.Type.SQL())
		}
		g.p("\tsql := runtime.SpliceOrder(%q, terms, %q, %q)", unordered, pgsql.OrderLead, pgsql.OrderSep)
		g.p("\treturn %s%sCache.Put(toks, &runtime.Stmt{SQL: sql, NArg: 2}).SQL", priv, form)
		g.p("}")
		g.p("")
	}
}
