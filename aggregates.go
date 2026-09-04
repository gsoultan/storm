package storm

import (
	"errors"
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// Named aggregations: a GROUP BY and the expressions over it, declared once.
//
//	func (o *Order) Aggregates(a *storm.Aggregates) {
//	    a.Named("Daily").
//	        ByExpr("Day", storm.DateTrunc("day", &o.PlacedAt)).
//	        Count("Orders").
//	        Count("Paid").Filter(storm.Eq(&o.Status, StatusPaid)).
//	        Sum(&o.Total, "Revenue").
//	        RowNumber("Rank", storm.Over().OrderByDesc(&o.Total))
//	}
//
//	rows, err := order.New().
//	    Where(order.PlacedAt.Gte(since)).   // call-site predicates still compose
//	    AllDaily(ctx, ex)                   // []order.DailyRow
//
// **Declared, not composed at the call site**, for the reason the library
// exists: a `GroupBy(...).Select(...)` chain assembled at run time has an
// unbounded set of result shapes, and a shape storm has not seen can have
// neither a generated scanner nor a compiled statement. Naming it keeps the
// whole thing inside the compilation thesis. The call-site predicates stay
// dynamic because those ARE bounded.
type Aggregates struct {
	// Exprs is the declaration-time expression vocabulary: a.Eq, a.DateTrunc,
	// a.Over and the rest. Embedded rather than exported at package level so a
	// declaration constructor cannot be reached from a query context.
	Exprs

	t   *Table
	out *[]*schema.Aggregate
}

// Aggregator is implemented by models that declare aggregations. Optional.
type Aggregator interface {
	Aggregates(*Aggregates)
}

// Out is a reference to a declared output, returned by the method that
// declared it.
//
// It replaces a string lookup. `Having(a.Gt(a.Out("Orders"), 0))` named an
// output that storm checked at build time; a handle is checked by the Go
// compiler, and it cannot name an output that has not been declared yet
// because you do not have one until it has.
type Out struct {
	b    *AggregateBuilder
	name string
}

// Filter restricts THIS aggregate to the rows matching cond —
// `count(*) FILTER (WHERE status = 'paid')`, which is both clearer and faster
// than `count(CASE WHEN ...)`.
//
// Attached to the output it filters rather than to "the last one declared",
// so moving a line cannot silently move the filter with it.
func (o Out) Filter(c Cond) Out {
	b := o.b
	if b == nil || b.dead {
		return o
	}
	t := b.term(o.name)
	if t == nil {
		b.fail("filter on %q: no such output", o.name)
		return o
	}
	if t.Expr.Kind != schema.ExprAgg {
		b.fail("Filter applies to an aggregate; %q is not one", o.name)
		return o
	}
	sc, err := b.resolveCond(c)
	if err != nil {
		b.fail("filter on %q: %w", o.name, err)
		return o
	}
	t.Expr.Filter = &sc
	return o
}

// OverWindow attaches a window to THIS aggregate — a moving total, or the
// classic "share of the group" without a self-join.
func (o Out) OverWindow(w *WindowSpec) Out {
	b := o.b
	if b == nil || b.dead {
		return o
	}
	t := b.term(o.name)
	if t == nil {
		b.fail("window on %q: no such output", o.name)
		return o
	}
	if t.Expr.Kind != schema.ExprAgg {
		b.fail("OverWindow applies to an aggregate; %q is not one", o.name)
		return o
	}
	if t.Expr.Distinct {
		b.fail("%q is a count(DISTINCT ...) and cannot take a window — PostgreSQL "+
			"rejects DISTINCT in an aggregate used as a window function", o.name)
		return o
	}
	win, err := b.resolveWindow(w)
	if err != nil {
		b.fail("window on %q: %w", o.name, err)
		return o
	}
	t.Expr.Over = win
	return o
}

// none is the handle a failed declaration returns: it refers to nothing and
// every method on it is a no-op, so a chain after an error neither panics nor
// reports the same mistake twice.
func (b *AggregateBuilder) none() Out { return Out{b: b} }

// term finds a declared output by name.
func (b *AggregateBuilder) term(name string) *schema.AggregateTerm {
	for i := range b.agg.Terms {
		if b.agg.Terms[i].As == name {
			return &b.agg.Terms[i]
		}
	}
	return nil
}

// AggregateBuilder accumulates one declaration.
type AggregateBuilder struct {
	a   *Aggregates
	agg *schema.Aggregate
	// dead stops a builder reporting the same declaration twice: once
	// something is rejected, every later call in the chain would produce a
	// second error about the same line.
	dead bool
	seen map[string]bool
}

// Named starts an aggregation. The generated type is this name plus "Row", so
// "Daily" becomes DailyRow in the table's package.
func (a *Aggregates) Named(name string) *AggregateBuilder {
	b := &AggregateBuilder{a: a, agg: &schema.Aggregate{Name: name}, seen: map[string]bool{}}
	if !isExportedIdent(name) {
		a.t.errs.add(fmt.Errorf(
			"%s: aggregate name %q must be a valid exported Go identifier — it becomes a type name",
			a.t.out.Name, name))
		b.dead = true
		return b
	}
	for _, ex := range *a.out {
		if ex.Name == name {
			a.t.errs.add(fmt.Errorf("%s: aggregate %q is declared twice", a.t.out.Name, name))
			b.dead = true
			return b
		}
	}
	*a.out = append(*a.out, b.agg)
	return b
}

// ---- grouping ---------------------------------------------------------------

// By groups by columns. The field name is derived from the column name.
func (b *AggregateBuilder) By(fieldPtrs ...any) *AggregateBuilder {
	for _, fp := range fieldPtrs {
		if b.dead {
			return b
		}
		c, err := b.a.t.resolve(fp)
		if err != nil {
			b.fail("%w", err)
			return b
		}
		b.addGroup(exportIdent(c.sc.Name), toTerm(fp))
	}
	return b
}

// ByExpr groups by an expression, which needs a name because
// date_trunc('day', placed_at) has no obvious one.
func (b *AggregateBuilder) ByExpr(as string, t Term) Out {
	if b.dead {
		return b.none()
	}
	if !isExportedIdent(as) {
		b.fail("grouping expression is named %q, which must be a valid exported Go identifier", as)
		return b.none()
	}
	return b.addGroup(as, t)
}

func (b *AggregateBuilder) addGroup(as string, t Term) Out {
	e, err := b.resolveTerm(t)
	if err != nil {
		b.fail("grouping %q: %w", as, err)
		return b.none()
	}
	// GROUP BY runs BEFORE aggregation, so an aggregate cannot appear in it —
	// PostgreSQL says "aggregate functions are not allowed in GROUP BY" and
	// the declaration is refused here instead, where the name of the offending
	// output is still known. The mistake is easy to make with arithmetic:
	// a ratio over two counts is an OUTPUT, not a grouping expression, and
	// Compute is where it goes.
	if containsAgg(e) {
		b.fail("grouping %q is computed from an aggregate, and GROUP BY runs before "+
			"aggregation — declare it with Compute, which makes it an output over the "+
			"group rather than part of the group's key", as)
		return b.none()
	}
	// Checked before the field-name clash so the message names the actual
	// mistake. Grouping by the same thing twice also produces two fields with
	// one name, but "you grouped by status twice" is the sentence that leads
	// to the fix.
	for _, ex := range b.agg.By {
		if exprEqual(ex.Expr, e) {
			b.fail("groups by %s twice", describeExpr(e))
			return b.none()
		}
	}
	if !b.claim(as) {
		return b.none()
	}
	b.agg.By = append(b.agg.By, schema.GroupTerm{Expr: e, As: as})
	return Out{b: b, name: as}
}

// describeExpr names an expression for an error message.
func describeExpr(e schema.Expr) string {
	switch e.Kind {
	case schema.ExprCol:
		return e.Col
	case schema.ExprFunc:
		return e.Fn + "(...)"
	}
	return "the same expression"
}

// Rollup turns the grouping into ROLLUP(...): every prefix of the grouping
// columns plus a grand total, in one pass instead of one query per level.
//
// Every grouping column becomes NULLABLE in the row type, because a subtotal
// row carries NULL for the columns it aggregated over. Use storm.Grouping to
// tell that NULL from one that was in the data.
func (b *AggregateBuilder) Rollup() *AggregateBuilder {
	return b.sets(&schema.GroupingSets{Kind: schema.SetsRollup})
}

// Cube is ROLLUP's every-combination sibling: 2ⁿ grouping sets.
func (b *AggregateBuilder) Cube() *AggregateBuilder {
	return b.sets(&schema.GroupingSets{Kind: schema.SetsCube})
}

// Sets declares explicit grouping sets by the names given to By/ByExpr. An
// empty set is the grand total row.
//
// This is the answer to N+1 queries per facet: one pass over the table
// produces every facet count, instead of one query per facet.
func (b *AggregateBuilder) Sets(sets ...[]string) *AggregateBuilder {
	if b.dead {
		return b
	}
	idx := make([][]int, 0, len(sets))
	for _, set := range sets {
		ints := make([]int, 0, len(set))
		for _, name := range set {
			at := -1
			for i, g := range b.agg.By {
				if g.As == name {
					at = i
				}
			}
			if at < 0 {
				b.fail("grouping set names %q, which is not one of its grouping columns", name)
				return b
			}
			ints = append(ints, at)
		}
		idx = append(idx, ints)
	}
	return b.sets(&schema.GroupingSets{Kind: schema.SetsExplicit, Sets: idx})
}

func (b *AggregateBuilder) sets(s *schema.GroupingSets) *AggregateBuilder {
	if b.dead {
		return b
	}
	if b.agg.Sets != nil {
		b.fail("declares its grouping sets twice")
		return b
	}
	if len(b.agg.By) == 0 {
		b.fail("has grouping sets but nothing to group by")
		return b
	}
	b.agg.Sets = s
	return b
}

// ---- aggregates -------------------------------------------------------------

// Count adds count(*): rows per group, never NULL.
func (b *AggregateBuilder) Count(as string) Out {
	return b.agged(schema.AggCount, star(), as)
}

// CountOf adds count(col), which counts rows where the column is NOT NULL — a
// different question from Count, and a common bug, so a different method.
func (b *AggregateBuilder) CountOf(fieldPtr any, as string) Out {
	return b.agged(schema.AggCount, toTerm(fieldPtr), as)
}

// CountDistinct adds count(DISTINCT col): how many DIFFERENT values the column
// takes, which is a third question again from Count (rows) and CountOf (rows
// where it is not null).
//
// DISTINCT is offered for count and nothing else. `sum(DISTINCT x)` and
// `avg(DISTINCT x)` are legal SQL and almost always a bug — the sum of the
// distinct values of a price column is not a number anyone wanted — so having
// them would cost more than it bought.
//
// It cannot be combined with a window: PostgreSQL rejects DISTINCT in an
// aggregate used as a window function, and storm refuses it at declaration
// rather than emitting SQL the server will not plan.
func (b *AggregateBuilder) CountDistinct(fieldPtr any, as string) Out {
	return b.aggedDist(schema.AggCount, toTerm(fieldPtr), as, true)
}

// Sum, Avg, Min and Max are the ordinary aggregates. All four are NULL over
// zero rows, so all four produce a nullable field.
func (b *AggregateBuilder) Sum(x any, as string) Out {
	return b.agged(schema.AggSum, toTerm(x), as)
}
func (b *AggregateBuilder) Avg(x any, as string) Out {
	return b.agged(schema.AggAvg, toTerm(x), as)
}
func (b *AggregateBuilder) Min(x any, as string) Out {
	return b.agged(schema.AggMin, toTerm(x), as)
}
func (b *AggregateBuilder) Max(x any, as string) Out {
	return b.agged(schema.AggMax, toTerm(x), as)
}

// GroupingOf adds GROUPING(cols...) — 1 when the row is a subtotal over those
// columns, 0 when the value is real.
func (b *AggregateBuilder) GroupingOf(as string, fieldPtrs ...any) Out {
	if b.dead {
		return b.none()
	}
	if b.agg.Sets == nil {
		b.fail("declares GROUPING %q but has no grouping sets, so nothing is ever a subtotal", as)
		return b.none()
	}
	return b.output(as, Term{kind: schema.ExprGrouping, args: toTerms(fieldPtrs)},
		schema.Type{Name: schema.TypeInt4}, false)
}

func (b *AggregateBuilder) agged(fn schema.AggFunc, arg Term, as string) Out {
	return b.aggedDist(fn, arg, as, false)
}

func (b *AggregateBuilder) aggedDist(fn schema.AggFunc, arg Term, as string, distinct bool) Out {
	if b.dead {
		return b.none()
	}
	if !isExportedIdent(as) {
		b.fail("%s(...) is named %q, which must be a valid exported Go identifier", fn, as)
		return b.none()
	}
	in := schema.Type{}
	if arg.kind != schema.ExprStar {
		e, err := b.resolveTerm(arg)
		if err != nil {
			b.fail("%s %q: %w", fn, as, err)
			return b.none()
		}
		in = e.Type
		// Refused HERE rather than at run time. PostgreSQL has no sum(text)
		// and no max(uuid); left to the server those are
		// "function max(uuid) does not exist" from a report that may only run
		// at month end.
		switch fn {
		case schema.AggSum, schema.AggAvg:
			if !schema.AggregatableSumAvg(in) {
				b.fail("%s %q: PostgreSQL has no %s() for %s", fn, as, fn, in.Name)
				return b.none()
			}
		case schema.AggMin, schema.AggMax:
			if !schema.AggregatableMinMax(in) {
				b.fail("%s %q: PostgreSQL has no %s() for %s", fn, as, fn, in.Name)
				return b.none()
			}
		}
	}
	rt, nullable, err := schema.AggregateResult(fn, in)
	if err != nil {
		b.fail("%s %q: %w", fn, as, err)
		return b.none()
	}
	resolved := schema.Expr{Kind: schema.ExprStar}
	if arg.kind != schema.ExprStar {
		var err error
		if resolved, err = b.resolveTerm(arg); err != nil {
			b.fail("%s %q: %w", fn, as, err)
			return b.none()
		}
	}
	e := schema.Expr{
		Kind: schema.ExprAgg, Fn: string(fn), Distinct: distinct,
		Args: []schema.Expr{resolved}, Type: rt, Nullable: nullable,
	}
	return b.push(as, e)
}

// ---- window functions -------------------------------------------------------

// SumOver, AvgOver, MinOver and MaxOver aggregate ACROSS THE GROUPS — the
// running total, the moving average, the high-water mark:
//
//	rev := b.Sum(&o.Total, "Revenue")
//	b.AvgOver(rev, "Moving7", a.Over().OrderByAsc(day).
//	    Rows(a.Preceding(6), a.CurrentRow()))
//
// The argument is a declared output, and that is the whole distinction. A
// grouped query has already collapsed its rows, so `sum(total) OVER (...)`
// reads a column that no longer exists per output row and PostgreSQL refuses
// it; `sum(sum(total)) OVER (...)` is the form that means "across the groups",
// and passing the handle is what produces it.
//
// Give them a frame. Without one PostgreSQL's default reaches from the start of
// the partition to the current row, which makes every one of these a RUNNING
// figure rather than a moving one.
func (b *AggregateBuilder) SumOver(x any, as string, w *WindowSpec) Out {
	return b.aggOver(schema.AggSum, x, as, w)
}
func (b *AggregateBuilder) AvgOver(x any, as string, w *WindowSpec) Out {
	return b.aggOver(schema.AggAvg, x, as, w)
}
func (b *AggregateBuilder) MinOver(x any, as string, w *WindowSpec) Out {
	return b.aggOver(schema.AggMin, x, as, w)
}
func (b *AggregateBuilder) MaxOver(x any, as string, w *WindowSpec) Out {
	return b.aggOver(schema.AggMax, x, as, w)
}

func (b *AggregateBuilder) aggOver(fn schema.AggFunc, x any, as string, w *WindowSpec) Out {
	if b.dead {
		return b.none()
	}
	if !isExportedIdent(as) {
		b.fail("%s(...) is named %q, which must be a valid exported Go identifier", fn, as)
		return b.none()
	}
	inner, err := b.resolveTerm(toTerm(x))
	if err != nil {
		b.fail("%s %q: %w", fn, as, err)
		return b.none()
	}
	rt, _, err := schema.AggregateResult(fn, inner.Type)
	if err != nil {
		b.fail("%s %q: %w", fn, as, err)
		return b.none()
	}
	win, err := b.resolveWindow(w)
	if err != nil {
		b.fail("window on %q: %w", as, err)
		return b.none()
	}
	return b.push(as, schema.Expr{
		Kind: schema.ExprAgg, Fn: string(fn),
		Args: []schema.Expr{inner}, Over: win,
		// Always nullable: a frame can be empty at a partition edge even when
		// every group has a value, and sum over no rows is NULL.
		Type: rt, Nullable: true,
	})
}

// RowNumber, Rank and DenseRank number rows within the window.
func (b *AggregateBuilder) RowNumber(as string, w *WindowSpec) Out {
	return b.windowed("row_number", nil, as, w)
}
func (b *AggregateBuilder) Rank(as string, w *WindowSpec) Out {
	return b.windowed("rank", nil, as, w)
}
func (b *AggregateBuilder) DenseRank(as string, w *WindowSpec) Out {
	return b.windowed("dense_rank", nil, as, w)
}

// PercentRank and CumeDist are the fractional ranks: where a row sits in its
// window as a number between 0 and 1, which is what a percentile report wants
// and what Rank cannot give without knowing the partition size.
//
// Both are float8 and never NULL — an empty window produces no rows to rank.
func (b *AggregateBuilder) PercentRank(as string, w *WindowSpec) Out {
	return b.windowed("percent_rank", nil, as, w)
}
func (b *AggregateBuilder) CumeDist(as string, w *WindowSpec) Out {
	return b.windowed("cume_dist", nil, as, w)
}

// Lag and Lead read the previous or next row in the window. Both are NULL at
// the partition edge however non-null the column is, so both produce a
// nullable field.
func (b *AggregateBuilder) Lag(x any, as string, w *WindowSpec) Out {
	return b.windowed("lag", toTerm(x), as, w)
}
func (b *AggregateBuilder) Lead(x any, as string, w *WindowSpec) Out {
	return b.windowed("lead", toTerm(x), as, w)
}

// FirstValue is the first row's value in the window.
func (b *AggregateBuilder) FirstValue(x any, as string, w *WindowSpec) Out {
	return b.windowed("first_value", toTerm(x), as, w)
}

// LastValue reads the last row of the window frame — and the frame, not the
// partition, is the point. PostgreSQL's default frame ends at the CURRENT ROW,
// so `last_value` without a frame returns the current row's own value, which
// is the single most reported surprise in window functions. Give it a frame
// that reaches the end:
//
//	a.Over().OrderByAsc(day).Rows(a.UnboundedPreceding(), a.UnboundedFollowing())
func (b *AggregateBuilder) LastValue(x any, as string, w *WindowSpec) Out {
	return b.windowed("last_value", toTerm(x), as, w)
}

func (b *AggregateBuilder) windowed(fn string, arg any, as string, w *WindowSpec) Out {
	if b.dead {
		return b.none()
	}
	if !isExportedIdent(as) {
		b.fail("%s(...) is named %q, which must be a valid exported Go identifier", fn, as)
		return b.none()
	}
	sig, ok := schema.WindowFuncs[fn]
	if !ok {
		b.fail("unknown window function %q", fn)
		return b.none()
	}
	if w == nil {
		b.fail("%s %q has no window — a window function without OVER has no meaning", fn, as)
		return b.none()
	}
	if sig.NeedsOrder && len(w.order) == 0 {
		b.fail("%s %q has a window with no ordering — %s over an unordered window "+
			"ranks by nothing and returns a different answer each run", fn, as, fn)
		return b.none()
	}
	var args []schema.Expr
	var in []schema.Type
	if arg != nil {
		e, err := b.resolveTerm(toTerm(arg))
		if err != nil {
			b.fail("%s %q: %w", fn, as, err)
			return b.none()
		}
		args = append(args, e)
		in = append(in, e.Type)
	}
	rt, err := sig.Result(in)
	if err != nil {
		b.fail("%s %q: %w", fn, as, err)
		return b.none()
	}
	win, err := b.resolveWindow(w)
	if err != nil {
		b.fail("%s %q: %w", fn, as, err)
		return b.none()
	}
	return b.push(as, schema.Expr{
		Kind: schema.ExprWindow, Fn: fn, Args: args, Over: win,
		Type: rt, Nullable: sig.AlwaysNullable,
	})
}

// ---- modifiers on the last term ---------------------------------------------

// Param declares a value the CALL supplies, for use inside a Filter or a
// Having:
//
//	since := b.Param("Since")
//	b.Count("Recent").Filter(a.Gte(&e.OccurredAt, since))
//
//	rows, err := event.New().AllRates(ctx, ex, time.Now().Add(-30*24*time.Hour))
//
// A FILTER is part of the declaration, so its condition is fixed at generate
// time — which makes "the last thirty days" unsayable, because that is
// relative to when the query runs. This is the narrow answer: the aggregation
// still has ONE shape, one compiled statement and one scanner; only a value
// varies.
//
// The type is inferred from the column the parameter is first compared with,
// so the generated signature cannot disagree with what it filters. Declared
// parameters are numbered before the call-site predicates, and appear in the
// generated function in declaration order.
func (b *AggregateBuilder) Param(name string) Term {
	if b.dead {
		return Term{err: errors.New("aggregate is already in error")}
	}
	if !isExportedIdent(name) {
		b.fail("parameter %q must be a valid exported Go identifier", name)
		return Term{err: errors.New("bad parameter name")}
	}
	for i, p := range b.agg.Params {
		if p.Name == name {
			return Term{kind: schema.ExprParam, param: i + 1}
		}
	}
	b.agg.Params = append(b.agg.Params, schema.Param{Name: name})
	return Term{kind: schema.ExprParam, param: len(b.agg.Params)}
}

// Having filters the GROUPS, after aggregation. A call-site Where filters the
// rows that go INTO the groups; these are different questions and mixing them
// up silently changes the answer.
func (b *AggregateBuilder) Having(c Cond) *AggregateBuilder {
	if b.dead {
		return b
	}
	if b.agg.Having != nil {
		b.fail("declares Having twice — combine them with storm.And")
		return b
	}
	sc, err := b.resolveCond(c)
	if err != nil {
		b.fail("having: %w", err)
		return b
	}
	b.agg.Having = &sc
	return b
}

// ---- resolution -------------------------------------------------------------

// Compute adds an output that is an EXPRESSION over the group — the ratio, the
// difference, the share — rather than a single aggregate:
//
//	orders := b.Count("Orders")
//	paid   := b.Count("Paid").Filter(a.Eq(&o.Status, "paid"))
//	b.Compute("PaidRate", a.Div(paid, a.NullIf(orders, a.Lit(0))))
//
// The NullIf is not decoration. It is the division-by-zero guard, and it sits
// in the expression instead of in a comment above it.
//
// The result type is whatever the expression resolves to, so a ratio over two
// counts is numeric — not the truncated integer PostgreSQL's `/` would give.
func (b *AggregateBuilder) Compute(as string, t Term) Out {
	if b.dead {
		return b.none()
	}
	if !isExportedIdent(as) {
		b.fail("computed output is named %q, which must be a valid exported Go identifier", as)
		return b.none()
	}
	e, err := b.resolveTerm(t)
	if err != nil {
		b.fail("computing %q: %w", as, err)
		return b.none()
	}
	return b.push(as, e)
}

// containsAgg reports whether an expression aggregates anywhere inside it.
func containsAgg(e schema.Expr) bool {
	if e.Kind == schema.ExprAgg || e.Kind == schema.ExprWindow {
		return true
	}
	for _, a := range e.Args {
		if containsAgg(a) {
			return true
		}
	}
	return false
}

func (b *AggregateBuilder) push(as string, e schema.Expr) Out {
	if !b.claim(as) {
		return b.none()
	}
	b.agg.Terms = append(b.agg.Terms, schema.AggregateTerm{Expr: e, As: as})
	return Out{b: b, name: as}
}

func (b *AggregateBuilder) output(as string, t Term, ty schema.Type, nullable bool) Out {
	e, err := b.resolveTerm(t)
	if err != nil {
		b.fail("%q: %w", as, err)
		return b.none()
	}
	e.Type, e.Nullable = ty, nullable
	return b.push(as, e)
}

// resolveTerm turns a declaration Term into a typed schema.Expr.
func (b *AggregateBuilder) resolveTerm(t Term) (schema.Expr, error) {
	if t.err != nil {
		return schema.Expr{}, t.err
	}
	if t.out != "" {
		// Any declared output: a grouping expression or an aggregate. Both are
		// things the surrounding query can legitimately name, and both expand
		// to their expression rather than to their alias — PostgreSQL resolves
		// SELECT aliases after grouping, so HAVING and a window's ORDER BY
		// cannot see them.
		for _, g := range b.agg.By {
			if g.As == t.out {
				return g.Expr, nil
			}
		}
		for _, term := range b.agg.Terms {
			if term.As == t.out {
				return term.Expr, nil
			}
		}
		return schema.Expr{}, fmt.Errorf(
			"storm.Out(%q) names no output declared so far — declare it before whatever refers to it", t.out)
	}
	switch t.kind {
	case schema.ExprParam:
		// Typed at the comparison, in resolveCond: a parameter alone has
		// nothing to take a type from.
		return schema.Expr{Kind: schema.ExprParam, Param: t.param - 1}, nil

	case schema.ExprStar:
		// Reachable only through resolveTerm's callers, all of which forbid a
		// bare star. Count handles its own argument before getting here.
		return schema.Expr{}, fmt.Errorf(
			"`*` is not a value: Count(name) already counts rows, and `* > 0` is not " +
				"a condition — use storm.Out(\"...\") to compare against a declared aggregate")

	case schema.ExprLit:
		return schema.Expr{Kind: schema.ExprLit, Lit: t.lit,
			Type: schema.Type{Name: t.lit.Kind}}, nil

	case schema.ExprCol:
		c, err := b.a.t.resolve(t.fp)
		if err != nil {
			return schema.Expr{}, err
		}
		return schema.Expr{Kind: schema.ExprCol, Col: c.sc.Name,
			Type: c.sc.Type, Nullable: !c.sc.NotNull}, nil

	case schema.ExprGrouping:
		args, _, anyNull, err := b.resolveArgs(t.args)
		if err != nil {
			return schema.Expr{}, err
		}
		_ = anyNull
		return schema.Expr{Kind: schema.ExprGrouping, Args: args,
			Type: schema.Type{Name: schema.TypeInt4}}, nil

	case schema.ExprBinary:
		args, in, anyNull, err := b.resolveArgs(t.args)
		if err != nil {
			return schema.Expr{}, err
		}
		rt, err := schema.BinaryResult(t.arith, in, t.scale)
		if err != nil {
			return schema.Expr{}, fmt.Errorf("%s: %w", t.arith, err)
		}
		return schema.Expr{
			Kind: schema.ExprBinary, Arith: t.arith, Args: args,
			Type: rt, Nullable: anyNull,
		}, nil

	case schema.ExprFunc:
		sig, ok := schema.Funcs[t.fn]
		if !ok {
			return schema.Expr{}, fmt.Errorf("unknown function %q", t.fn)
		}
		args, in, anyNull, err := b.resolveArgs(t.args)
		if err != nil {
			return schema.Expr{}, err
		}
		if sig.Args >= 0 && len(args) != sig.Args {
			return schema.Expr{}, fmt.Errorf("%s wants %d argument(s), got %d", t.fn, sig.Args, len(args))
		}
		rt, err := sig.Result(in)
		if err != nil {
			return schema.Expr{}, fmt.Errorf("%s: %w", t.fn, err)
		}
		nullable := sig.NullableIfAnyArgIs && anyNull
		switch t.fn {
		case "nullif":
			nullable = true // its entire purpose
		case "coalesce":
			// Non-null exactly when some argument cannot be null.
			nullable = true
			for _, a := range args {
				if !a.Nullable {
					nullable = false
					break
				}
			}
		}
		return schema.Expr{Kind: schema.ExprFunc, Fn: t.fn, Args: args,
			Type: rt, Nullable: nullable}, nil
	}
	return schema.Expr{}, fmt.Errorf("unsupported expression")
}

func (b *AggregateBuilder) resolveArgs(ts []Term) ([]schema.Expr, []schema.Type, bool, error) {
	out := make([]schema.Expr, 0, len(ts))
	in := make([]schema.Type, 0, len(ts))
	anyNull := false
	for _, t := range ts {
		e, err := b.resolveTerm(t)
		if err != nil {
			return nil, nil, false, err
		}
		out = append(out, e)
		in = append(in, e.Type)
		anyNull = anyNull || e.Nullable
	}
	return out, in, anyNull, nil
}

func (b *AggregateBuilder) resolveCond(c Cond) (schema.Cond, error) {
	if c.err != nil {
		return schema.Cond{}, c.err
	}
	switch c.kind {
	case schema.CondCmp:
		l, err := b.resolveTerm(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		r, err := b.resolveTerm(c.right)
		if err != nil {
			return schema.Cond{}, err
		}
		// A declared parameter takes the type of whatever it is compared with,
		// before comparability is judged — it has none of its own.
		if err := b.typeParam(&l, r); err != nil {
			return schema.Cond{}, err
		}
		if err := b.typeParam(&r, l); err != nil {
			return schema.Cond{}, err
		}
		// A literal declared as text next to an enum column is the ordinary
		// case and PostgreSQL casts it; anything else being compared across
		// unrelated types is a mistake worth naming.
		if err := comparable(l, r); err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: schema.CondCmp, Op: c.op, Left: l, Right: r}, nil

	case schema.CondIsNull, schema.CondIsNotNull:
		l, err := b.resolveTerm(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: c.kind, Left: l}, nil

	default:
		if len(c.args) == 0 {
			return schema.Cond{}, fmt.Errorf("empty condition")
		}
		args := make([]schema.Cond, 0, len(c.args))
		for _, a := range c.args {
			sa, err := b.resolveCond(a)
			if err != nil {
				return schema.Cond{}, err
			}
			args = append(args, sa)
		}
		return schema.Cond{Kind: c.kind, Args: args}, nil
	}
}

// wholePartition reports a frame that covers every row of the partition, which
// is the one frame whose meaning does not depend on the ordering.
func wholePartition(f *schema.Frame) bool {
	return f.Start.Kind == schema.UnboundedPreceding &&
		f.End.Kind == schema.UnboundedFollowing
}

// typeParam gives a declared parameter the type of the thing beside it.
func (b *AggregateBuilder) typeParam(p *schema.Expr, other schema.Expr) error {
	if p.Kind != schema.ExprParam {
		return nil
	}
	if other.Kind == schema.ExprParam {
		return errors.New(
			"two parameters are compared with each other, so neither has a type " +
				"to take — compare a parameter with a column")
	}
	decl := &b.agg.Params[p.Param]
	if decl.Type.Name == "" {
		decl.Type = other.Type
		p.Type = other.Type
		return nil
	}
	if decl.Type.Name != other.Type.Name {
		return fmt.Errorf(
			"parameter %q is compared with %s here and %s elsewhere — it is one "+
				"argument at the call, so it is one Go type",
			decl.Name, other.Type.Name, decl.Type.Name)
	}
	p.Type = decl.Type
	return nil
}

func (b *AggregateBuilder) resolveWindow(w *WindowSpec) (*schema.Window, error) {
	if w.err != nil {
		return nil, w.err
	}
	out := &schema.Window{Frame: w.frame}
	if w.frame != nil && len(w.order) == 0 && !wholePartition(w.frame) {
		// A frame counts rows in the window's order. With no ORDER BY there is
		// no order to count in, so the frame selects an arbitrary set that can
		// differ between runs of the same query — a wrong answer that does not
		// look like one.
		//
		// UNBOUNDED PRECEDING to UNBOUNDED FOLLOWING is the exception, and it
		// is the reason this is not a blanket rule: that frame is the whole
		// partition however the rows are ordered, so it is exactly as
		// deterministic without an ORDER BY as with one. It is also the way to
		// say "the total for this partition" next to a per-row value — the
		// share-of-group query — and refusing it would send that back to SQL.
		return nil, errors.New(
			"a window frame needs an ORDER BY: without one the rows have no order to " +
				"count along, and the frame picks an arbitrary set\n" +
				"       (a frame of UnboundedPreceding to UnboundedFollowing is the whole " +
				"partition and needs no ordering)")
	}
	for _, p := range w.partition {
		e, err := b.resolveTerm(p)
		if err != nil {
			return nil, err
		}
		out.PartitionBy = append(out.PartitionBy, e)
	}
	for _, o := range w.order {
		e, err := b.resolveTerm(o.term)
		if err != nil {
			return nil, err
		}
		out.OrderBy = append(out.OrderBy, schema.OrderTerm{Expr: e, Desc: o.desc})
	}
	return out, nil
}

// comparable rejects a comparison whose two sides cannot be compared.
func comparable(l, r schema.Expr) error {
	if l.Type.Name == "" || r.Type.Name == "" {
		return nil // a star or an untyped node; nothing to check
	}
	if l.Type.Name == r.Type.Name {
		return nil
	}
	// An enum column compared with a text literal is the everyday case.
	if l.Type.Enum && r.Type.Name == schema.TypeText {
		return nil
	}
	if r.Type.Enum && l.Type.Name == schema.TypeText {
		return nil
	}
	if numericish(l.Type.Name) && numericish(r.Type.Name) {
		return nil
	}
	if textish(l.Type.Name) && textish(r.Type.Name) {
		return nil
	}
	return fmt.Errorf("compares %s with %s, which PostgreSQL will not do", l.Type.Name, r.Type.Name)
}

func numericish(n string) bool {
	switch n {
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8,
		schema.TypeNumeric, schema.TypeFloat4, schema.TypeFloat8:
		return true
	}
	return false
}

func textish(n string) bool {
	return n == schema.TypeText || n == schema.TypeVarchar
}

func (b *AggregateBuilder) claim(name string) bool {
	if b.seen[name] {
		b.fail("has two outputs named %q — each becomes a field of the same struct", name)
		return false
	}
	b.seen[name] = true
	return true
}

func (b *AggregateBuilder) fail(format string, a ...any) {
	b.a.t.errs.add(fmt.Errorf("%s: aggregate %q "+format,
		append([]any{b.a.t.out.Name, b.agg.Name}, a...)...))
	b.dead = true
}
