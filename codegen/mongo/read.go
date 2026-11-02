package codegenmongo

import (
	"strconv"

	"github.com/gsoultan/storm/compile/mongo"
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
