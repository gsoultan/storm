package codegenmongo

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

func (g *gen) reads() {
	fs := g.fields()
	g.p(`// The statement caches, one per scope: [0] the live documents, [1] every
// document, for a query that said WithDeleted.
type caches [2]*runtime.TreeCache

func newCaches() caches { return caches{runtime.NewTreeCache(), runtime.NewTreeCache()} }

var (
	findCache   = newCaches()
	countCache  = newCaches()
	existsCache = newCaches()
	deleteCache = newCaches()
	updateCache = newCaches()
	insertCache = runtime.NewMaskCache()
	mutCache    = runtime.NewMaskCache()
)

// Shapes reports how many distinct read structures have compiled.
func Shapes() int { return findCache[0].Shapes() + findCache[1].Shapes() }

// ShapeFlushes should read zero forever; see docs/GENERATED.md.
func ShapeFlushes() int { return findCache[0].Flushes() + findCache[1].Flushes() }

// filter splices the predicate tokens into a filter document, holes from
// first; scope 0 on a soft-delete table puts the liveness test in front.
func filter(toks []runtime.Tok, first, scope int) (string, int, error) {
	f, n, err := mgsplice.Splice(toks, fragOf, punct, first)
	if err == nil && scope == 0 && liveOpen != "" {
		f = liveOpen + f + liveClose
	}
	return f, n, err
}

// filtered compiles a command whose filter sits between head and tail, its
// holes numbered from first.
func filtered(cs caches, scope int, toks []runtime.Tok, head, tail string, first int, sorted bool) *runtime.Stmt {
	c := cs[scope]
	if st := c.Get(toks); st != nil {
		return st
	}
	f, n, err := filter(toks, first, scope)
	st := &runtime.Stmt{Err: err, NArg: first - 1 + n}
	if err == nil {
		s := head + f
		if sorted {
			s += %s + mgsplice.Sort(toks, sortOf, %s, %s, %s, defaultSort)
		}
		st.SQL = s + tail
	}
	return c.Put(toks, st)
}
`, strconv.Quote(mongo.FindSort), strconv.Quote(mongo.SortOpen), strconv.Quote(mongo.SortSep), strconv.Quote(mongo.SortClose))
	g.p("var (")
	g.p("\tfindHead   = %s", strconv.Quote(mongo.FindHead(g.t.Name, fs)))
	g.p("\tcountHead  = %s", strconv.Quote(mongo.CountHead(g.t.Name)))
	g.p("\texistsHead = %s", strconv.Quote(mongo.ExistsHead(g.t.Name)))
	g.p("\tdeleteHead = %s", strconv.Quote(g.deleteAllHead()))
	g.p(")")
	g.p("")
	g.p(`// SQL is the command this query runs — the name every target shares, for
// logging and tests. It is a MongoDB command document here.
func (q Query) SQL() string {
	return filtered(findCache, q.scope(), q.stream(), findHead, %s, %d, true).SQL
}

func (q Query) args(lead ...any) []any {
	args := make([]any, 0, len(lead)+int(q.nv))
	args = append(args, lead...)
	return append(args, q.vals[:q.nv]...)
}

// All runs the query and appends every row to dst.
func (q Query) All(ctx context.Context, ex runtime.Executor, dst []Row) ([]Row, error) {
	err := q.Each(ctx, ex, func(r Row) error { dst = append(dst, r); return nil })
	return dst, err
}

// List is All into a new slice.
func (q Query) List(ctx context.Context, ex runtime.Executor) ([]Row, error) {
	return q.All(ctx, ex, nil)
}

// Each runs the query and calls fn with each row in turn. The row is fn's to
// keep. An error from fn stops the read and is returned as it is.
func (q Query) Each(ctx context.Context, ex runtime.Executor, fn func(Row) error) error {
	if q.err != nil {
		return q.err
	}
	if q.limit <= 0 {
		return nil
	}
	st := filtered(findCache, q.scope(), q.stream(), findHead, %s, %d, true)
	if st.Err != nil {
		return st.Err
	}
	rows, err := ex.Query(ctx, st.SQL, q.args(q.limit, q.offset))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Row
		if err := scan(rows.RawValues(), &r); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Rows is Each as a range-over-func iterator.
func (q Query) Rows(ctx context.Context, ex runtime.Executor) func(yield func(Row, error) bool) {
	return func(yield func(Row, error) bool) {
		stop := errors.New("stop")
		err := q.Each(ctx, ex, func(r Row) error {
			if !yield(r, nil) {
				return stop
			}
			return nil
		})
		if err != nil && err != stop {
			yield(Row{}, err)
		}
	}
}

// One returns the first row, and false when there is none.
func (q Query) One(ctx context.Context, ex runtime.Executor) (Row, bool, error) {
	var out Row
	found := false
	q.limit = 1
	err := q.Each(ctx, ex, func(r Row) error { out, found = r, true; return nil })
	return out, found, err
}

// Count counts the matching rows, ignoring Limit and Offset.
func (q Query) Count(ctx context.Context, ex runtime.Executor) (int64, error) {
	if q.err != nil {
		return 0, q.err
	}
	st := filtered(countCache, q.scope(), q.stream(), countHead, %s, 1, false)
	if st.Err != nil {
		return 0, st.Err
	}
	rows, err := ex.Query(ctx, st.SQL, q.args())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		// $count produces no document at all when nothing matched.
		return 0, rows.Err()
	}
	n, err := bsondec.Int8(rows.RawValues()[0])
	if err != nil {
		return 0, err
	}
	return n, rows.Err()
}

// Exists reports whether any row matches, reading one key and sorting
// nothing.
func (q Query) Exists(ctx context.Context, ex runtime.Executor) (bool, error) {
	if q.err != nil {
		return false, q.err
	}
	st := filtered(existsCache, q.scope(), q.stream(), existsHead, %s, 1, false)
	if st.Err != nil {
		return false, st.Err
	}
	rows, err := ex.Query(ctx, st.SQL, q.args())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

var errDeletedWrite = errors.New("storm: WithDeleted is for reads; UpdateAll and DeleteAll act on the live documents only")

func (q Query) writeAllErr() error {
	switch {
	case q.err != nil:
		return q.err
	case !q.preds():
		return runtime.ErrUnfilteredWrite
	case q.paged:
		return runtime.ErrPagedWrite
	case q.scope() != 0:
		return errDeletedWrite
	}
	return nil
}
`, strconv.Quote(mongo.FindTail), mongo.FindFirstArg,
		strconv.Quote(mongo.FindTail), mongo.FindFirstArg,
		strconv.Quote(mongo.CountTail), strconv.Quote(mongo.ExistsTail))
	if g.soft >= 0 {
		g.p(`// DeleteAll marks every live document the query matches deleted, in ONE
// command, and reports how many it marked. Like Delete it removes nothing;
// documents already marked are not stamped again.
// The query needs a predicate (runtime.ErrUnfilteredWrite) and no Limit or
// Offset (runtime.ErrPagedWrite).
func (q Query) DeleteAll(ctx context.Context, ex runtime.Executor) (int64, error) {
	if err := q.writeAllErr(); err != nil {
		return 0, err
	}
	st := filtered(deleteCache, 0, q.stream(), deleteHead, %s, 1, false)
	if st.Err != nil {
		return 0, st.Err
	}
	return ex.Exec(ctx, st.SQL, q.args())
}
`, strconv.Quote(mongo.UpdateTail))
		return
	}
	run := "ex.Exec(ctx, st.SQL, q.args())"
	if g.plan != nil {
		run = "mgcascade.Delete(ctx, ex, cascade, filtered(filterCache, 0, q.stream(), \"\", \"\", 1, false).SQL, q.args(), st.SQL)"
		g.p("// filterCache holds DeleteAll's filter alone, for the cascade's first read.")
		g.p("var filterCache = newCaches()")
		g.p("")
	}
	g.p(`// DeleteAll deletes every matching row and reports how many.
// The query needs a predicate (runtime.ErrUnfilteredWrite) and no Limit or
// Offset (runtime.ErrPagedWrite).
func (q Query) DeleteAll(ctx context.Context, ex runtime.Executor) (int64, error) {
	if err := q.writeAllErr(); err != nil {
		return 0, err
	}
	st := filtered(deleteCache, 0, q.stream(), deleteHead, %s, 1, false)
	if st.Err != nil {
		return 0, st.Err
	}
	return %s
}
`, strconv.Quote(mongo.DeleteTail), run)
}

// deleteAllHead is DeleteAll's command up to its filter: a delete, or on a
// soft-delete table an update that marks.
func (g *gen) deleteAllHead() string {
	if g.soft < 0 {
		return mongo.DeleteHead(g.t.Name)
	}
	return mongo.UpdateHead(g.t.Name, g.markDoc())
}

// markDoc stamps the soft-delete column with the server's clock.
func (g *gen) markDoc() string {
	return `[{"$set":{` + mongo.Str(g.stored(g.soft)) + `:"$$NOW"}}]`
}

// projections emits each declared projection: a row type of its columns and
// terminals that read only them — the same filter, sort and paging, a
// narrower projection document.
func (g *gen) projections() {
	for _, pr := range g.t.Projections {
		var cols []int
		var fields []string
		for _, name := range pr.Columns {
			i := g.index(name)
			cols = append(cols, i)
			fields = append(fields, g.stored(i))
		}
		row := pr.Name + "Row"
		lower := strings.ToLower(pr.Name[:1]) + pr.Name[1:]
		g.p("// %s is the %q projection: the same read, %d field(s) instead of the", row, pr.Name, len(cols))
		g.p("// whole document.")
		g.p("type %s struct {", row)
		for _, i := range cols {
			g.p("\t%s %s", g.field(i), g.kind[i].fieldType(g.cols[i]))
		}
		g.p("}")
		g.p("")
		g.p("var (")
		g.p("\t%sHead  = %s", lower, strconv.Quote(mongo.FindHead(g.t.Name, fields)))
		g.p("\t%sCache = newCaches()", lower)
		g.p(")")
		g.p("")
		g.scanCols("scan"+pr.Name, row, strconv.Itoa(len(cols)), cols)
		g.p("")
		g.p(`// All%[1]s runs the query projected to %[2]s. Predicates, ordering, limit
// and offset apply exactly as on All: a projection changes what a row
// carries, never which rows qualify.
func (q Query) All%[1]s(ctx context.Context, ex runtime.Executor) ([]%[2]s, error) {
	if q.err != nil {
		return nil, q.err
	}
	if q.limit <= 0 {
		return nil, nil
	}
	st := filtered(%[3]sCache, q.scope(), q.stream(), %[3]sHead, %[4]s, %[5]d, true)
	if st.Err != nil {
		return nil, st.Err
	}
	rows, err := ex.Query(ctx, st.SQL, q.args(q.limit, q.offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []%[2]s
	for rows.Next() {
		var r %[2]s
		if err := scan%[1]s(rows.RawValues(), &r); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// One%[1]s is All%[1]s stopped at one row.
func (q Query) One%[1]s(ctx context.Context, ex runtime.Executor) (%[2]s, bool, error) {
	out, err := q.Limit(1).All%[1]s(ctx, ex)
	if err != nil || len(out) == 0 {
		return %[2]s{}, false, err
	}
	return out[0], true, nil
}
`, pr.Name, row, lower, strconv.Quote(mongo.FindTail), mongo.FindFirstArg)
	}
}

// aggregates emits each declared aggregation as a $group pipeline over the
// query's filter — the subset compile/mgddl admits — with SQL's answers: a
// sum or average over no values is NULL, not 0; an integer average is a
// decimal rounded half away from zero to its declared scale; NULL groups
// together and sorts last ascending; and with no grouping columns there is
// one row even over no documents.
func (g *gen) aggregates() {
	for _, a := range g.t.Aggregates {
		g.aggregate(a)
	}
}

func (g *gen) aggregate(a *schema.Aggregate) {
	row := a.Name + "Row"
	lower := strings.ToLower(a.Name[:1]) + a.Name[1:]
	type out struct {
		as       string
		k        kind
		nullable bool
	}
	var outs []out
	var key, acc, proj []string
	for i, b := range a.By {
		c := g.t.Column(b.Expr.Col)
		k, _ := kindOf(c)
		outs = append(outs, out{b.As, k, !c.NotNull && !k.slice})
		key = append(key, mongo.Str("k"+strconv.Itoa(i))+`:`+orNull(g.stored(g.index(c.Name))))
		proj = append(proj, mongo.Str(b.As)+`:"$_id.k`+strconv.Itoa(i)+`"`)
	}
	for i, term := range a.Terms {
		e := term.Expr
		res := &schema.Column{Type: e.Type, NotNull: !e.Nullable}
		k, _ := kindOf(res)
		outs = append(outs, out{term.As, k, k.nullable(res)})
		t := "t" + strconv.Itoa(i)
		var f, in string
		var col *schema.Column
		if e.Args[0].Kind == schema.ExprCol {
			col = g.t.Column(e.Args[0].Col)
			f = orNull(g.stored(g.index(col.Name)))
		}
		present := `{"$cond":[{"$eq":[` + f + `,null]},0,1]}`
		// nullIfNone is v, or null when the group held no value to aggregate.
		nullIfNone := func(v string) string {
			acc = append(acc, mongo.Str("n"+strconv.Itoa(i))+`:{"$sum":`+present+`}`)
			return `{"$cond":[{"$eq":["$n` + strconv.Itoa(i) + `",0]},null,` + v + `]}`
		}
		switch {
		case e.Fn == string(schema.AggCount) && col == nil:
			acc = append(acc, mongo.Str(t)+`:{"$sum":1}`)
			in = `"$` + t + `"`
		case e.Fn == string(schema.AggCount) && e.Distinct:
			acc = append(acc, mongo.Str(t)+`:{"$addToSet":`+f+`}`)
			in = `{"$size":{"$filter":{"input":"$` + t + `","cond":{"$ne":["$$this",null]}}}}`
		case e.Fn == string(schema.AggCount):
			acc = append(acc, mongo.Str(t)+`:{"$sum":`+present+`}`)
			in = `"$` + t + `"`
		case e.Fn == string(schema.AggSum):
			v := f
			if col.Type.Name == schema.TypeInt8 {
				// PostgreSQL sums int8 as numeric, which cannot overflow;
				// MongoDB would sum longs into a double past 2^63.
				v = `{"$toDecimal":` + f + `}`
			}
			acc = append(acc, mongo.Str(t)+`:{"$sum":`+v+`}`)
			in = nullIfNone(`"$` + t + `"`)
		case e.Fn == string(schema.AggAvg) && e.Type.Name == schema.TypeNumeric:
			acc = append(acc, mongo.Str(t)+`:{"$sum":{"$toDecimal":`+f+`}}`)
			in = nullIfNone(roundHalfAway(`{"$divide":["$`+t+`","$n`+strconv.Itoa(i)+`"]}`, e.Type.Scale))
		case e.Fn == string(schema.AggAvg):
			acc = append(acc, mongo.Str(t)+`:{"$avg":`+f+`}`)
			in = `"$` + t + `"`
		default: // min, max
			acc = append(acc, mongo.Str(t)+`:{"$`+e.Fn+`":`+f+`}`)
			in = `"$` + t + `"`
		}
		proj = append(proj, mongo.Str(term.As)+`:`+in)
	}
	id := "null"
	if len(key) > 0 {
		id = `{` + strings.Join(key, ",") + `}`
	}
	// The order: the declared one, then the grouping columns as a tiebreak;
	// each term behind a null flag, so NULL sorts where SQL puts it — last
	// ascending, first descending.
	type term struct {
		as   string
		desc bool
	}
	var order []term
	seen := map[string]bool{}
	for _, o := range a.OrderBy {
		order = append(order, term{o.As, o.Desc})
		seen[o.As] = true
	}
	for _, b := range a.By {
		if !seen[b.As] {
			order = append(order, term{b.As, false})
		}
	}
	var flags, sort []string
	for i, o := range order {
		dir := "1"
		if o.desc {
			dir = "-1"
		}
		flag := "_null" + strconv.Itoa(i)
		flags = append(flags, mongo.Str(flag)+`:{"$cond":[{"$eq":["$`+o.as+`",null]},1,0]}`)
		sort = append(sort, mongo.Str(flag)+":"+dir, mongo.Str(o.as)+":"+dir)
	}
	tail := `},{"$group":{"_id":` + id
	if len(acc) > 0 {
		tail += `,` + strings.Join(acc, ",")
	}
	tail += `}},{"$project":{` + strings.Join(proj, ",") + `}}`
	if len(flags) > 0 {
		tail += `,{"$addFields":{` + strings.Join(flags, ",") + `}},{"$sort":{` + strings.Join(sort, ",") + `}}`
	}
	tail += `,{"$skip":` + mongo.Arg(2) + `},{"$limit":` + mongo.Arg(1) + `}]}`
	head := `{"aggregate":` + mongo.Str(g.t.Name) + `,"cursor":{},"pipeline":[{"$match":`

	g.p("// %s is the %q aggregation over %s: one row per group, the grouping", row, a.Name, g.t.Name)
	g.p("// columns first, ordered as declared or by the grouping columns.")
	g.p("type %s struct {", row)
	for _, o := range outs {
		ft := o.k.goType
		if o.nullable {
			ft = "runtime.Null[" + ft + "]"
		}
		g.p("\t%s %s", o.as, ft)
	}
	g.p("}")
	g.p("")
	g.p("var (")
	g.p("\t%sHead  = %s", lower, strconv.Quote(head))
	g.p("\t%sTail  = %s", lower, strconv.Quote(tail))
	g.p("\t%sCache = newCaches()", lower)
	g.p(")")
	g.p("")
	g.p("func scan%s(rv [][]byte, r *%s) error {", a.Name, row)
	g.p("\tif len(rv) != %d {", len(outs))
	g.p("\t\treturn runtime.ErrShape")
	g.p("\t}")
	g.p("\tvar err error")
	for j, o := range outs {
		g.decodeField(j, o.as, o.k, o.nullable, g.t.Name+"."+a.Name+"."+o.as)
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	scope := "q.scope()"
	if a.AcrossDeleted {
		scope = "1"
	}
	empty := ""
	if len(a.By) == 0 {
		empty = `
	if len(out) == 0 && q.offset == 0 {
		// No grouping columns: SQL answers one row over no rows — the
		// counts 0 and everything else NULL — where $group answers none.
		out = append(out, ` + row + `{})
	}`
	}
	g.p(`// All%[1]s runs the %[1]q aggregation. Predicates filter the documents that
// go INTO the groups; Limit and Offset page the groups. Order is refused —
// the rows are groups, ordered as the declaration says.
func (q Query) All%[1]s(ctx context.Context, ex runtime.Executor) ([]%[2]s, error) {
	if q.err != nil {
		return nil, q.err
	}
	if q.sorted {
		return nil, errors.New("storm: Order() on the %[1]s aggregation — its rows are groups, ordered as declared")
	}
	if q.limit <= 0 {
		return nil, nil
	}
	st := filtered(%[3]sCache, %[4]s, q.stream(), %[3]sHead, %[3]sTail, %[5]d, false)
	if st.Err != nil {
		return nil, st.Err
	}
	rows, err := ex.Query(ctx, st.SQL, q.args(q.limit, q.offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []%[2]s
	for rows.Next() {
		var r %[2]s
		if err := scan%[1]s(rows.RawValues(), &r); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}%[6]s
	return out, nil
}
`, a.Name, row, lower, scope, mongo.FindFirstArg, empty)
}

// orNull reads a field as null when it is missing, so a missing field and a
// null one are one value — SQL's NULL — to a group key or an aggregate.
func orNull(field string) string { return `{"$ifNull":["$` + field + `",null]}` }

// roundHalfAway rounds a decimal to scale places the way PostgreSQL's
// round(numeric) does — half away from zero, where $round rounds half to
// even — and leaves exactly that many places, which an integer times
// 10^-scale has.
func roundHalfAway(v string, scale int) string {
	pow := "1" + strings.Repeat("0", scale)
	unit := "1"
	if scale > 0 {
		unit = "0." + strings.Repeat("0", scale-1) + "1"
	}
	return `{"$let":{"vars":{"v":` + v + `},"in":{"$multiply":[{"$cond":[{"$lt":["$$v",0]},-1,1]},` +
		`{"$floor":{"$add":[{"$multiply":[{"$abs":"$$v"},{"$toDecimal":"` + pow + `"}]},{"$toDecimal":"0.5"}]}},` +
		`{"$toDecimal":"` + unit + `"}]}}}`
}
