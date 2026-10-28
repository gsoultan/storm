package codegen

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

func (g *gen) joins() {
	joinedAgg := false
	for _, a := range g.t.Aggregates {
		if len(a.Joins) > 0 {
			joinedAgg = true
		}
	}
	if len(g.t.Joins) == 0 && !joinedAgg {
		return
	}
	g.joinLowering()
	for _, j := range g.t.Joins {
		g.join(j)
	}
}

// joinLowering emits the predicates a join's call site adds, with every
// column qualified by its table. Joined tables share column names — every
// one of them has an id — so the bare name the single-table read uses is
// ambiguous there, and the server refuses the statement.
func (g *gen) joinLowering() {
	table := g.lw.Ident(g.t.Name)
	g.p("// joinFragTable is fragTable for a join: every column named with its table.")
	g.fragTable("joinFragTable", func(col string) string { return table + "." + g.lw.Ident(col) })
	g.p("func joinFragOf(op, col uint32) runtime.Frag {")
	g.p("\tif int(col) >= len(joinFragTable) || int(op) >= len(joinFragTable[0]) {")
	g.p("\t\treturn runtime.Frag{}")
	g.p("\t}")
	g.p("\treturn joinFragTable[col][op]")
	g.p("}")
	g.p("")
	g.p("// joinLowering is lowering with qualified predicates. A join takes no")
	g.p("// call-site order and no cursor, so qualifying the predicates is all")
	g.p("// it needs.")
	g.p("var joinLowering = func() runtime.Lowering {")
	g.p("\tl := lowering")
	g.p("\tl.Frag = joinFragOf")
	g.p("\treturn l")
	g.p("}()")
	g.p("")
	for _, j := range g.t.Joins {
		if narrowed(j) {
			g.p("// JoinLowering is joinLowering, exported for the composers that run")
			g.p("// this table's joins with narrowed CTEs.")
			g.p("func JoinLowering() runtime.Lowering { return joinLowering }")
			g.p("")
			break
		}
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

	if narrowed(j) {
		// The composer in the context package runs it: its CTEs take queries
		// of other tables, which this package does not import.
		if g.lw.JoinPieces == nil {
			g.err = fmt.Errorf("codegen: join %s narrows a CTE, which %s cannot spell yet", j.Name, g.lw.name)
			return
		}
		g.joinScan(j, cols)
		g.p("// Scan%s decodes one %s row, for the composer that runs the join.", j.Name, j.Name)
		g.p("func Scan%s(rv %s, r *%sRow, sl *runtime.Slab) error { return scan%s(rv, r, sl) }",
			j.Name, g.dec.rowsType(), j.Name, j.Name)
		g.p("")
		return
	}

	prefix, err := g.lw.JoinSelect(g.t.Name, j, func(c schema.CTE) (string, string) {
		return g.cteSQL(c)
	}, func(tb, al string) string { return string(liveIn(g.lw, g.s, tb, al)) })
	if err != nil {
		g.err = fmt.Errorf("codegen: join %s: %w", j.Name, err)
		return
	}
	if g.err != nil {
		return
	}
	g.p("const %sPrefix = %s", low, lit(prefix))
	suffix, err := g.lw.JoinSuffix(j)
	if err != nil {
		g.err = fmt.Errorf("codegen: join %s: %w", j.Name, err)
		return
	}
	g.p("const %sSuffix = %s", low, lit(suffix))
	declared, err := g.lw.JoinDeclaredWhere(j, string(liveIn(g.lw, g.s, g.t.Name, g.t.Name)))
	if err != nil {
		g.err = fmt.Errorf("codegen: join %s: %w", j.Name, err)
		return
	}
	if w := declared; w != "" {
		// The declared predicate is ANDed with whatever the caller adds, so a
		// declaration that says "only fulfilled orders" cannot be widened at a
		// call site. That is the point of declaring it there.
		g.p("const %sWhere = %s", low, lit(w))
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
	if declared != "" {
		g.p("\treturn c.Put(toks, runtime.SpliceTreeWhere(%sPrefix, %sWhere, toks, joinLowering, suffix))", low, low)
	} else {
		g.p("\treturn c.Put(toks, runtime.SpliceTree(%sPrefix, toks, joinLowering, suffix))", low)
	}
	g.p("}")
	g.p("")

	g.joinScan(j, cols)

	g.p("// All%s runs the %q join. Call-site predicates apply to %s and compose",
		j.Name, j.Name, g.t.Name)
	g.p("// with whatever the declaration already filtered.")
	g.p("func (q Query) All%s(ctx context.Context, ex "+g.execType()+") ([]%sRow, error) {", j.Name, j.Name)
	g.p("\tvar sl runtime.Slab")
	g.p("\treturn q.All%sInto(ctx, ex, nil, &sl)", j.Name)
	g.p("}")
	g.p("")
	g.p("// All%sInto lets the caller own the output slice and the arena.", j.Name)
	g.p("func (q Query) All%sInto(ctx context.Context, ex "+g.execType()+", dst []%sRow, sl *runtime.Slab) ([]%sRow, error) {",
		j.Name, j.Name, j.Name)
	g.p("\tif err := q.Err(); err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	// The declared ORDER BY is part of the statement; a call-site Order would
	// name an unqualified column of a multi-table result.
	g.p("\tif q.no > 0 {")
	g.p("\t\treturn dst, err%sOrdered", j.Name)
	g.p("\t}")
	g.p("\tif q.lock != 0 {")
	g.p("\t\treturn dst, err%sLocked", j.Name)
	g.p("\t}")
	g.p("\tvar buf [%d]runtime.Tok", g.streamBuf())
	g.p("\tst := %sStmtFor(q.preds(&buf), q.offset > 0)", low)
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn dst, st.Err")
	g.p("\t}")
	g.p("\tsl.Reserve(st.SlabHint())")
	g.p("\tb := binders.Get()")
	g.p("\tdefer putBinder(b)")
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, q.bind(b))"))
	g.p("\tif err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tfor rows.Next() {")
	g.p("\t\tdst = append(dst, %sRow{})", j.Name)
	g.p("\t\tif err := scan%s(rows.%s(), &dst[len(dst)-1], sl); err != nil {", j.Name, g.dec.rowsAccessor())
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tst.ObserveSlab(sl.Size())")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
	g.p("var err%sLocked = errors.New(", j.Name)
	g.p("\t%q)", g.lw.LockRefusedJoined())
	g.p("")
	g.p("var err%sOrdered = errors.New(", j.Name)
	g.p("\t%q)", "storm: Order() on a join — its ordering is declared, because a column name "+
		"in a multi-table result is ambiguous; sort the returned slice if you need another order")
	g.p("")
}

// joinScan emits the decoder for one row of a join.
func (g *gen) joinScan(j *schema.Join, cols []aggCol) {
	fallible := false
	for _, c := range cols {
		if fallibleIn(c.col, g.dec) {
			fallible = true
		}
	}
	g.p("func scan%s(rv %s, r *%sRow, sl *runtime.Slab) error {", j.Name, g.dec.rowsType(), j.Name)
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
}

// narrowed reports a join with a CTE its call site narrows.
func narrowed(j *schema.Join) bool {
	for _, c := range j.CTEs {
		if c.Narrowed {
			return true
		}
	}
	return false
}

// cteSQL renders a CTE's body: the declared aggregation, whole.
func (g *gen) cteSQL(c schema.CTE) (string, string) {
	t := g.s.Table(c.Table)
	if t == nil {
		g.err = fmt.Errorf("codegen: CTE %s names table %s, which is not in this context", c.Alias, c.Table)
		return "", ""
	}
	for _, a := range t.Aggregates {
		if a.Name != c.Aggregate {
			continue
		}
		// Through the lowering, not PostgreSQL's. Unreachable on MySQL today —
		// mysql.JoinSelect refuses a join with a CTE before this is called —
		// but a leak that is only unreachable is a leak waiting for the
		// refusal to be lifted.
		sel, err := g.lw.AggregateSelect(t.Name, a)
		if err != nil {
			g.err = err
			return "", ""
		}
		suf, err := g.lw.AggregateSuffix(a)
		if err != nil {
			g.err = err
			return "", ""
		}
		// The aggregation's own read leaves the marked rows out, and so does
		// the CTE made of it: a join is not a way round the soft delete.
		declared, err := aggDeclared(g.lw, g.s, t, a)
		if err != nil {
			g.err = err
			return "", ""
		}
		return sel + pgsql.Live(declared).Clause(), suf
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

// narrowedJoin is one composer to emit: a join with a CTE its call site
// narrows, run from the context package because the CTEs take queries of
// other tables.
type narrowedJoin struct {
	t *schema.Table
	j *schema.Join
}

// narrowedJoins enumerates them, in table order.
func narrowedJoins(s *schema.Schema, names []string) []narrowedJoin {
	var out []narrowedJoin
	for _, n := range names {
		t := s.Table(n)
		if t == nil {
			continue
		}
		for _, j := range t.Joins {
			if narrowed(j) {
				out = append(out, narrowedJoin{t: t, j: j})
			}
		}
	}
	return out
}

// emitNarrowedJoin writes the composer for one join with narrowed CTEs.
//
//	rows, err := store.DefinitionShown(
//	    definition.New().Where(definition.Name.ILike(p)),   // the join's own rows
//	    release.New().Where(release.ActivateAt.Lte(now)),   // the "live" CTE's
//	).Limit(50).All(ctx, ex)
//
// One statement. Each CTE's predicates are spliced into its own WHERE with its
// own package's lowering, numbered on from the CTE before it, and the main
// predicates follow with the declaring package's qualified lowering — so no
// stream is rebased and no package imports another.
func (g *gen) emitNarrowedJoin(lw lowering, s *schema.Schema, nj narrowedJoin) (pkgs []string, err error) {
	t, j := nj.t, nj.j
	pkg, err := PackageName(t.GoName, t.Name)
	if err != nil {
		return nil, err
	}
	if lw.JoinPieces == nil {
		return nil, fmt.Errorf("codegen: table %s join %s narrows a CTE, which %s cannot spell yet", t.Name, j.Name, lw.name)
	}
	type cte struct {
		c        schema.CTE
		pkg      string
		head     string
		declared string
		tail     string
	}
	var ctes []cte
	var aggErr error
	aggFor := func(c schema.CTE) (string, string) {
		ct := s.Table(c.Table)
		if ct == nil {
			aggErr = fmt.Errorf("codegen: CTE %s names table %s, which is not in this context", c.Alias, c.Table)
			return "", ""
		}
		for _, a := range ct.Aggregates {
			if a.Name != c.Aggregate {
				continue
			}
			sel, err := lw.AggregateSelect(ct.Name, a)
			if err == nil {
				var suf string
				suf, err = lw.AggregateSuffix(a)
				cpkg, perr := PackageName(ct.GoName, ct.Name)
				if err == nil {
					err = perr
				}
				declared, derr := aggDeclared(lw, s, ct, a)
				if err == nil {
					err = derr
				}
				ctes = append(ctes, cte{c: c, pkg: cpkg, declared: declared})
				if err == nil {
					return sel, suf
				}
			}
			aggErr = err
			return "", ""
		}
		aggErr = fmt.Errorf("codegen: CTE %s names aggregate %s.%s, which does not exist", c.Alias, c.Table, c.Aggregate)
		return "", ""
	}
	pieces, sel, count := lw.JoinPieces(t.Name, j, aggFor, func(tb, al string) string { return string(liveIn(lw, s, tb, al)) })
	if aggErr != nil {
		return nil, aggErr
	}
	for i := range ctes {
		ctes[i].head, ctes[i].tail = pieces[i][0], pieces[i][1]
	}
	suffix, err := lw.JoinSuffix(j)
	if err != nil {
		return nil, err
	}
	where, err := lw.JoinDeclaredWhere(j, string(liveIn(lw, s, t.Name, t.Name)))
	if err != nil {
		return nil, err
	}
	pkgs = append(pkgs, pkg)

	name := exportName(t.GoName) + j.Name
	q, low := name+"Query", lowerFirst(name)
	var params, fields []string
	for i, c := range ctes {
		if !c.c.Narrowed {
			continue
		}
		param := lowerFirst(exportName(c.c.Alias))
		if isGoKeyword(param) || param == "q" {
			param += "Q"
		}
		params = append(params, fmt.Sprintf("%s %s.Query", param, c.pkg))
		pkgs = append(pkgs, c.pkg)
		fields = append(fields, fmt.Sprintf("w%d: %s", i, param))
	}

	g.p("// %s runs the %q join of %s: one query for its own rows and one for", q, j.Name, t.Name)
	g.p("// each CTE it narrows, whose predicates choose the rows that CTE")
	g.p("// aggregates. Build it with %s.", name)
	g.p("type %s struct {", q)
	g.p("\tq %s.Query", pkg)
	for i, c := range ctes {
		if c.c.Narrowed {
			g.p("\tw%d %s.Query // %s", i, c.pkg, c.c.Alias)
		}
	}
	g.p("}")
	g.p("")
	g.p("// %s composes the %q join: q chooses %s's rows, and each", name, j.Name, t.Name)
	g.p("// further query the rows of the CTE it is named for, in declaration order.")
	g.p("func %s(q %s.Query, %s) %s {", name, pkg, strings.Join(params, ", "), q)
	g.p("\treturn %s{q: q, %s}", q, strings.Join(fields, ", "))
	g.p("}")
	g.p("")
	g.p("// %sCTEs are each CTE's head, declared predicate and tail.", low)
	g.p("var %sCTEs = [%d][3]string{", low, len(ctes))
	for _, c := range ctes {
		g.p("\t{%s, %s, %s},", lit(c.head), lit(c.declared), lit(c.tail))
	}
	g.p("}")
	g.p("")
	g.p("const (")
	g.p("\t%sSelect = %s", low, lit(sel))
	g.p("\t%sCount  = %s", low, lit(count))
	g.p("\t%sWhere  = %s", low, lit(where))
	g.p("\t%sSuffix = %s", low, lit(suffix))
	g.p("\t%sLimit  = %s", low, lit(lw.LimitOffsetSuffix(false)))
	g.p("\t%sPaged  = %s", low, lit(lw.LimitOffsetSuffix(true)))
	g.p(")")
	g.p("")
	g.p("var %sCache = runtime.NewTreeCache()", low)
	g.p("")
	g.p("var err%s = errors.New(", name)
	g.p("\t%q)", "storm: the "+j.Name+" join takes no Order() and no row lock, and its narrowing "+
		"queries take predicates alone — no Order, Limit, Offset or row lock")
	g.p("")

	g.p("func (h %s) err() error {", q)
	g.p("\tif err := h.q.Err(); err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tif h.q.Ordered() || h.q.Locked() {")
	g.p("\t\treturn err%s", name)
	g.p("\t}")
	for i, c := range ctes {
		if !c.c.Narrowed {
			continue
		}
		g.p("\tif err := h.w%d.Err(); err != nil {", i)
		g.p("\t\treturn err")
		g.p("\t}")
		g.p("\tif h.w%d.Ordered() || h.w%d.Shaped() {", i, i)
		g.p("\t\treturn err%s", name)
		g.p("\t}")
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")

	buf := g.composedBuf()
	g.p("// stmt compiles the streams once per shape. The key is a flag, each")
	g.p("// stream's length, then the streams.")
	g.p("func (h %s) stmt(count, paged bool) *runtime.Stmt {", q)
	var streams []string
	for i, c := range ctes {
		if !c.c.Narrowed {
			continue
		}
		g.p("\tvar b%d [%d]runtime.Tok", i, buf)
		g.p("\tt%d := h.w%d.PredToks(b%d[:0])", i, i, i)
		streams = append(streams, fmt.Sprintf("t%d", i))
	}
	g.p("\tvar bq [%d]runtime.Tok", buf)
	g.p("\ttq := h.q.PredToks(bq[:0])")
	streams = append(streams, "tq")
	g.p("\tvar kb [%d]runtime.Tok", (len(streams))*(buf+1)+1)
	g.p("\tflag := runtime.Tok(0)")
	g.p("\tif count {")
	g.p("\t\tflag = 1")
	g.p("\t} else if paged {")
	g.p("\t\tflag = 2")
	g.p("\t}")
	g.p("\tkey := append(kb[:0], flag)")
	for _, st := range streams {
		g.p("\tkey = append(key, runtime.Tok(len(%s)))", st)
		g.p("\tkey = append(key, %s...)", st)
	}
	g.p("\tif st := %sCache.Get(key); st != nil {", low)
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\tvar (")
	g.p("\t\tsql string")
	g.p("\t\tn   int")
	g.p("\t\terr error")
	g.p("\t)")
	g.p("\tvar st *runtime.Stmt")
	for i, c := range ctes {
		toks, lwr := "nil", pkg+".Lowering()"
		if c.c.Narrowed {
			toks, lwr = fmt.Sprintf("t%d", i), c.pkg+".Lowering()"
		}
		g.p("\tst = runtime.SpliceTreeWhereFrom(%sCTEs[%d][0], %sCTEs[%d][1], %s, %s, %sCTEs[%d][2], n)",
			low, i, low, i, toks, lwr, low, i)
		g.p("\tsql, n = sql+st.SQL, st.NArg")
		g.p("\tif err == nil {")
		g.p("\t\terr = st.Err")
		g.p("\t}")
	}
	g.p("\thead, tail := %sSelect, %sSuffix+%sLimit", low, low, low)
	g.p("\tif paged {")
	g.p("\t\ttail = %sSuffix + %sPaged", low, low)
	g.p("\t}")
	g.p("\tif count {")
	g.p("\t\thead, tail = %sCount, \"\"", low)
	g.p("\t}")
	g.p("\tst = runtime.SpliceTreeWhereFrom(head, %sWhere, tq, %s.JoinLowering(), tail, n)", low, pkg)
	g.p("\tsql, n = sql+st.SQL, st.NArg")
	g.p("\tif err == nil {")
	g.p("\t\terr = st.Err")
	g.p("\t}")
	g.p("\treturn %sCache.Put(key, &runtime.Stmt{SQL: sql, NArg: n, Err: err})", low)
	g.p("}")
	g.p("")

	bind := func(count bool) {
		g.p("\targs := make([]any, 0, st.NArg)")
		for i, c := range ctes {
			if !c.c.Narrowed {
				continue
			}
			g.p("\tb%d := %s.GetBinder()", i, c.pkg)
			g.p("\tdefer %s.PutBinder(b%d)", c.pkg, i)
			g.p("\targs = h.w%d.BindPreds(b%d, args)", i, i)
		}
		g.p("\tpb := %s.GetBinder()", pkg)
		g.p("\tdefer %s.PutBinder(pb)", pkg)
		g.p("\targs = h.q.BindPreds(pb, args)")
		if !count {
			g.p("\targs = h.q.BindPaging(pb, args)")
		}
	}

	g.p("// All runs the join: its rows in the declared order, paged by q.")
	g.p("func (h %s) All(ctx context.Context, ex %s) ([]%s.%sRow, error) {", q, g.execType(), pkg, j.Name)
	g.p("\tif err := h.err(); err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tst := h.stmt(false, h.q.Paged())")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn nil, st.Err")
	g.p("\t}")
	bind(false)
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tvar sl runtime.Slab")
	g.p("\tvar out []%s.%sRow", pkg, j.Name)
	g.p("\tfor rows.Next() {")
	g.p("\t\tout = append(out, %s.%sRow{})", pkg, j.Name)
	g.p("\t\tif err := %s.Scan%s(rows.%s(), &out[len(out)-1], &sl); err != nil {", pkg, j.Name, g.dec.rowsAccessor())
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn out, rows.Err()")
	g.p("}")
	g.p("")
	g.p("// Count counts the rows All would return before paging.")
	g.p("func (h %s) Count(ctx context.Context, ex %s) (int64, error) {", q, g.execType())
	g.p("\tif err := h.err(); err != nil {")
	g.p("\t\treturn 0, err")
	g.p("\t}")
	g.p("\tst := h.stmt(true, false)")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn 0, st.Err")
	g.p("\t}")
	bind(true)
	g.p("\trows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("\tif err != nil {")
	g.p("\t\treturn 0, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tif !rows.Next() {")
	g.p("\t\treturn 0, rows.Err()")
	g.p("\t}")
	g.p("\treturn %s(rows.%s()[0]), rows.Err()", g.dec.q("Int8"), g.dec.rowsAccessor())
	g.p("}")
	g.p("")
	return pkgs, nil
}
