package codegen

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
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

	g.p("// Greatest-n-per-group needs a strict total order: add the primary key as a")
	g.p("// final term if the natural ordering has ties.")
	g.p("var errNoTopOrder = errors.New(")
	g.p("\t%q)", "storm: a per-parent limit needs an ordering — without one "+
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

	g.p("// %s fetches at most n rows for each id, ordered by order, in one", name)
	g.p("// round trip. The lowering was chosen at generate time.")
	g.p("func %s(ctx context.Context, ex "+g.execType()+", ids []%s, n int64, order ...Sort) ([]Row, error) {", name, keyGo)
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
		g.p("func %s%s(ctx context.Context, ex "+g.execType()+", ids []%s, n int64, order ...Sort) ([]Row, error) {", name, form, keyGo)
		g.p("\treturn %sRun(ctx, ex, %s%sSQL(order), ids, n)", priv, priv, form)
		g.p("}")
		g.p("")
	}

	g.p("// %sRun executes one lowered statement.", priv)
	g.p("func %sRun(ctx context.Context, ex "+g.execType()+", sql string, ids []%s, n int64) ([]Row, error) {", priv, keyGo)
	g.p("\tif len(ids) == 0 || n <= 0 {")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\tif sql == \"\" {")
	g.p("\t\treturn nil, errNoTopOrder")
	g.p("\t}")
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, sql, []any{ids, n})"))
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tvar sl runtime.Slab")
	g.p("\tout := make([]Row, 0, int64(len(ids))*n)")
	g.p("\tfor rows.Next() {")
	g.p("\t\tout = append(out, Row{})")
	g.p("\t\tif err := scan(rows.%s(), &out[len(out)-1], &sl); err != nil {", g.dec.rowsAccessor())
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
			unordered = g.lw.TopNWindow(g.t.Name, cols, key, g.lw.KeyType(kc), string(g.live()))
		} else {
			unordered = g.lw.TopNLateral(g.t.Name, cols, key, g.lw.KeyType(kc), string(g.live()))
		}
		g.p("\tsql := runtime.SpliceOrder(%q, terms, %q, %q)", unordered, g.lw.OrderLead, pgsql.OrderSep)
		g.p("\treturn %s%sCache.Put(toks, &runtime.Stmt{SQL: sql, NArg: 2}).SQL", priv, form)
		g.p("}")
		g.p("")
	}
}

// distinctOn emits AllDistinctOn: the query's rows, keeping the first of each
// run of its leading n ORDER BY terms. "Each process's newest setting" is
// one read rather than a read of the whole history folded in Go.
//
// A terminal, not a modifier on Query, so no other terminal has to refuse a
// state it cannot honour: a count, an existence probe or a write over
// DISTINCT ON would each mean something different from the read.
func (g *gen) distinctOn() {
	if g.lw.DistinctOn == nil {
		return
	}
	open, sep, tail := g.lw.DistinctOn(g.t.Name, readableCols(g.t))
	g.p("const (")
	g.p("\tdistinctOpen = %q", open)
	g.p("\tdistinctSep  = %q", sep)
	g.p("\tdistinctTail = %q", tail)
	g.p(")")
	g.p("")
	g.p("// distinctCaches are AllDistinctOn's, without and with an offset. The")
	g.p("// key is the stream and then n, which the stream alone does not carry.")
	g.p("var distinctCaches = [2]*runtime.TreeCache{runtime.NewTreeCache(), runtime.NewTreeCache()}")
	g.p("")
	g.p("var errDistinctOn = errors.New(")
	g.p("\t%q)", "storm: AllDistinctOn keeps the first row of each run of the query's first n "+
		"order terms; n must be between one and the number of terms Order set, and the "+
		"query must not lock rows")
	g.p("")
	g.p("// distinctStmtFor compiles one (stream, n), once.")
	g.p("func distinctStmtFor(key, toks []runtime.Tok, n int, withOffset bool) *runtime.Stmt {")
	g.p("\ti, suffix := 0, limitSuffix")
	g.p("\tif withOffset {")
	g.p("\t\ti, suffix = 1, limitOffsetSuffix")
	g.p("\t}")
	g.p("\tc := distinctCaches[i]")
	g.p("\tif st := c.Get(key); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\tprefix := distinctOpen")
	g.p("\tfor _, t := range toks {")
	g.p("\t\tif t.Kind() != runtime.KOrder || n == 0 {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tif prefix != distinctOpen {")
	g.p("\t\t\tprefix += distinctSep")
	g.p("\t\t}")
	g.p("\t\tprefix += identTable[t.Col()]")
	g.p("\t\tn--")
	g.p("\t}")
	g.p("\treturn c.Put(key, %s)", g.splice("prefix+distinctTail", "toks, lowering, suffix"))
	g.p("}")
	g.p("")
	g.p("// AllDistinctOn keeps the first row, in the query's order, for each distinct")
	g.p("// value of its first n order terms. Limit and Offset count the rows kept;")
	g.p("// n outside the explicit order, or a row lock, is refused.")
	g.p("func (q Query) AllDistinctOn(ctx context.Context, ex %s, n int, dst []Row) ([]Row, error) {", g.execType())
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tif n < 1 || n > int(q.no) || q.lock != 0 {")
	g.p("\t\treturn dst, errDistinctOn")
	g.p("\t}")
	g.p("\tvar buf [%d]runtime.Tok", g.streamBuf())
	g.p("\ttoks := q.stream(&buf)")
	g.p("\tvar kb [%d]runtime.Tok", g.streamBuf()+1)
	g.p("\tkey := kb[:copy(kb[:], toks)+1]")
	g.p("\tkey[len(key)-1] = runtime.Tok(n)")
	g.p("\tst := distinctStmtFor(key, toks, n, q.offset > 0)")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn dst, st.Err")
	g.p("\t}")
	g.p("\tvar sl runtime.Slab")
	g.p("\tsl.Reserve(st.SlabHint())")
	g.p("\tb := binders.Get()")
	g.p("\tdefer putBinder(b)")
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, q.bind(b))"))
	g.p("\tif err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tfor rows.Next() {")
	g.p("\t\tdst = append(dst, Row{})")
	g.p("\t\tif err := scan(rows.%s(), &dst[len(dst)-1], &sl); err != nil {", g.dec.rowsAccessor())
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
}

// turns emits AllTurns for a table that takes turns (t.TakesTurns): the
// round-robin read, in one statement. The groups query's predicates choose
// the groups and the receiver's choose, order and count the rows, so the
// statement splices two streams; each is spliced as often as the text
// repeats it, with the same ordinals, and bound once.
func (g *gen) turns() {
	if g.t.Turns == "" || g.lw.Turns == nil {
		return
	}
	var group *schema.Column
	for _, c := range g.t.Columns {
		if c.Name == g.t.Turns {
			group = c
		}
	}
	var gc colInfo
	for _, c := range g.cols {
		if c.col == group {
			gc = c
		}
	}
	if group == nil || gc.col == nil {
		return
	}
	live := ""
	if g.t.SoftDeletes() {
		live = g.lw.SoftDeleteWhere(g.t.SoftDelete)
	}
	head, groups, rows := g.lw.Turns(g.t.Name, group.Name, readableCols(g.t), !group.NotNull, live)
	g.p("// TurnRow is a row and its turn: its place among its group's rows, in")
	g.p("// the order AllTurns was given.")
	g.p("type TurnRow struct {")
	g.p("\tRow")
	g.p("\tTurn int64")
	g.p("}")
	g.p("")
	g.p("const turnsHead = %q", head)
	g.p("")
	g.p("// turnsGroups and turnsRows are each splice's declared predicate and")
	g.p("// the text after it.")
	g.p("var turnsGroups = [4][2]string{")
	for _, p := range groups {
		g.p("\t{%q, %q},", p[0], p[1])
	}
	g.p("}")
	g.p("")
	g.p("var turnsRows = [%d][2]string{", len(rows))
	for _, p := range rows {
		g.p("\t{%q, %q},", p[0], p[1])
	}
	g.p("}")
	g.p("")
	g.p("var turnsCache = runtime.NewTreeCache()")
	g.p("")
	g.p("var errTurns = errors.New(")
	g.p("\t%q)", "storm: AllTurns deals out the rows of a query with no Offset and no row "+
		"lock, from groups chosen by predicates alone, and wants at least one group")
	g.p("")
	g.p("// turnsStmtFor compiles one pair of streams, once. key is the groups'")
	g.p("// stream behind its length, then the rows'.")
	g.p("func turnsStmtFor(key, gt, rt []runtime.Tok) *runtime.Stmt {")
	g.p("\tif st := turnsCache.Get(key); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\tsql, n := turnsHead, 3")
	g.p("\tvar err error")
	g.p("\tfor _, p := range turnsGroups {")
	g.p("\t\tst := runtime.SpliceTreeWhereFrom(\"\", p[0], gt, lowering, p[1], 3)")
	g.p("\t\tsql, n = sql+st.SQL, st.NArg")
	g.p("\t\tif err == nil {")
	g.p("\t\t\terr = st.Err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treserved := n")
	g.p("\tfor _, p := range turnsRows {")
	g.p("\t\tst := runtime.SpliceTreeWhereFrom(\"\", p[0], rt, lowering, p[1], reserved)")
	g.p("\t\tsql, n = sql+st.SQL, st.NArg")
	g.p("\t\tif err == nil {")
	g.p("\t\t\terr = st.Err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tvar terms []string")
	g.p("\tfor _, t := range rt {")
	g.p("\t\tif t.Kind() == runtime.KOrder {")
	g.p("\t\t\tterms = append(terms, lowering.Order(t.Op(), t.Col()))")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\t// Each window numbers by the order, and the result is in turn, then")
	g.p("\t// the order.")
	g.p("\tfor range turnsRows {")
	g.p("\t\tsql = runtime.SpliceOrder(sql, terms, lowering.OB.Lead, lowering.OB.Sep)")
	g.p("\t}")
	g.p("\tsql = runtime.SpliceOrder(sql, terms, lowering.OB.Sep, lowering.OB.Sep)")
	g.p("\treturn turnsCache.Put(key, &runtime.Stmt{SQL: sql, NArg: n, Err: err})")
	g.p("}")
	g.p("")
	g.p("// AllTurns deals the query's rows out by %s, a turn at a time: each", exportName(group.Name))
	g.p("// group's first rows numbered in the query's order, and every group's")
	g.p("// first turn ahead of anyone's second. groups chooses the groups by its")
	g.p("// predicates alone; at most n of them are read, from the first at or")
	g.p("// after from, wrapping round to the start. The query's limit caps each")
	g.p("// group's rows and the rows returned.")
	if !group.NotNull {
		g.p("//")
		g.p("// The rows with no %s are one more group.", exportName(group.Name))
	}
	g.p("func (q Query) AllTurns(ctx context.Context, ex %s, groups Query, from %s, n int, dst []TurnRow) ([]TurnRow, error) {",
		g.execType(), gc.goBase)
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tif err := groups.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tif n < 1 || q.offset != 0 || q.lock != 0 || groups.lock != 0 {")
	g.p("\t\treturn dst, errTurns")
	g.p("\t}")
	g.p("\tvar gb, rb [%d]runtime.Tok", g.streamBuf())
	g.p("\tgt, rt := groups.preds(&gb), q.stream(&rb)")
	g.p("\tvar kb [%d]runtime.Tok", 2*g.streamBuf()+1)
	g.p("\tkb[0] = runtime.Tok(len(gt))")
	g.p("\tkey := kb[:1+copy(kb[1:], gt)]")
	g.p("\tkey = key[:len(key)+copy(kb[len(key):], rt)]")
	g.p("\tst := turnsStmtFor(key, gt, rt)")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn dst, st.Err")
	g.p("\t}")
	g.p("\tgbind, rbind := binders.Get(), binders.Get()")
	g.p("\tdefer putBinder(gbind)")
	g.p("\tdefer putBinder(rbind)")
	g.p("\targs := make([]any, 0, st.NArg)")
	g.p("\targs = append(args, from, int64(n), q.limit)")
	g.p("\targs = append(args, groups.bindPreds(gbind)...)")
	g.p("\targs = append(args, q.bindPreds(rbind)...)")
	g.p("\tvar sl runtime.Slab")
	g.p("\tsl.Reserve(st.SlabHint())")
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("\tif err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tfor rows.Next() {")
	g.p("\t\trv := rows.%s()", g.dec.rowsAccessor())
	g.p("\t\tdst = append(dst, TurnRow{})")
	g.p("\t\tr := &dst[len(dst)-1]")
	g.p("\t\tif err := scan(rv, &r.Row, &sl); err != nil {")
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t\tr.Turn = runtime.Int8(rv[len(rv)-1])")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
}
