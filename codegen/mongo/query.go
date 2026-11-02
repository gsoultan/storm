package codegenmongo

import (
	"strconv"

	"github.com/gsoultan/storm/compile/mongo"
)

func (g *gen) query() {
	ops := mongo.Ops()
	g.p("// fragTab is every comparison on every column, lowered at generate time.")
	g.p("var fragTab = [%d][nCols]mgsplice.Frag{", len(ops)+1)
	g.p("\t{},")
	for _, op := range ops {
		g.p("\t{ // %s", op)
		for i := range g.cols {
			a, b, arg, ok := mongo.Frag(op, g.stored(i))
			if !ok {
				g.p("\t\t{},")
				continue
			}
			g.p("\t\t{A: %s, B: %s, Arg: %v},", strconv.Quote(a), strconv.Quote(b), arg)
		}
		g.p("\t},")
	}
	g.p("}")
	g.p("")
	g.p("func fragOf(op, col uint32) mgsplice.Frag { return fragTab[op][col] }")
	g.p("")
	g.p("var sortTab = [2][nCols]string{")
	for _, desc := range []bool{false, true} {
		g.p("\t{")
		for i := range g.cols {
			g.p("\t\t%s,", strconv.Quote(mongo.SortTerm(g.stored(i), desc)))
		}
		g.p("\t},")
	}
	g.p("}")
	g.p("")
	g.p("func sortOf(dir, col uint32) string { return sortTab[dir&1][col] }")
	g.p("")
	g.p("var punct = mgsplice.Punctuation{And: %s, Or: %s, Sep: %s, End: %s, Empty: %s}",
		strconv.Quote(mongo.AndOpen), strconv.Quote(mongo.OrOpen), strconv.Quote(mongo.GroupSep),
		strconv.Quote(mongo.GroupEnd), strconv.Quote(mongo.NoFilter))
	g.p("")
	g.p("// defaultSort is the primary key: a read with no ordering has none, and")
	g.p("// paging one is a bug waiting for a plan change to expose it.")
	g.p("var defaultSort = %s", strconv.Quote(mongo.SortOpen+mongo.SortTerm(mongo.IDField, false)+mongo.SortClose))
	g.p("")
	if g.soft >= 0 {
		g.p("// liveOpen and liveClose AND the soft-delete test in FRONT of the caller's")
		g.p("// filter, in every read and every write this package generates: a")
		g.p("// marked document is gone, and no call site can widen that by")
		g.p("// forgetting a predicate. Only a query that says WithDeleted is spliced")
		g.p("// without it.")
		g.p("const (")
		g.p("\tliveOpen  = %s", strconv.Quote(`{"$and":[{`+mongo.Str(g.stored(g.soft))+`:null},`))
		g.p("\tliveClose = %s", strconv.Quote(`]}`))
		g.p(")")
	} else {
		g.p("const liveOpen, liveClose = \"\", \"\"")
	}
	g.p("")
	g.p("const maxToks = 32")
	g.p("")
	deleted := ""
	if g.widens() {
		deleted = "\n\tdeleted bool"
	}
	g.p(`// Query is a value type: composing one copies it, and q2 := q1.Where(x)
// never changes q1. Predicates are a postfix token stream, the same one the
// SQL back ends key their statements on.
type Query struct {
	toks   [maxToks]runtime.Tok
	vals   [maxToks]any
	nt, nv uint8
	limit  int64
	offset int64
	paged  bool
	sorted bool
	err    error%s
}

// New starts a query: every document, the first 1000, by primary key.
func New() Query { return Query{limit: 1000} }

// Limit caps the rows returned. Zero returns none, as LIMIT 0 does.
func (q Query) Limit(n int64) Query { q.limit = n; q.paged = true; return q }

// Offset skips n rows. The server still walks them; prefer a predicate on
// the sort key for deep pages.
func (q Query) Offset(n int64) Query { q.offset = n; q.paged = true; return q }

// Err reports a query that could not be built.
func (q Query) Err() error { return q.err }

var errTooComplex = errors.New("storm: query has more predicates than its token buffer holds")

func (q *Query) push(t runtime.Tok) {
	if int(q.nt) >= maxToks {
		q.err = errTooComplex
		return
	}
	q.toks[q.nt] = t
	q.nt++
}

func (q *Query) leaf(p Pred) {
	if p.err != nil {
		if q.err == nil {
			q.err = p.err
		}
		return
	}
	q.push(runtime.MakeLeaf(uint32(p.op), uint32(p.col)))
	if fragTab[p.op][p.col].Arg && q.err == nil {
		q.vals[q.nv] = p.v
		q.nv++
	}
}

// Where ANDs its predicates onto the query.
func (q Query) Where(ps ...Pred) Query {
	for _, p := range ps {
		q.leaf(p)
	}
	return q
}

// WhereIf adds p only when cond holds. p is built either way, so it must
// not dereference something cond guards — use WhenSet for that.
func (q Query) WhereIf(cond bool, p Pred) Query {
	if cond {
		return q.Where(p)
	}
	return q
}

// WhenSet adds f(*v) only when v is non-nil.
func WhenSet[T any](q Query, v *T, f func(T) Pred) Query {
	if v == nil {
		return q
	}
	return q.Where(f(*v))
}

// Any ORs its predicates, and ANDs the group onto the query.
func (q Query) Any(ps ...Pred) Query {
	if len(ps) == 0 {
		return q
	}
	for _, p := range ps {
		q.leaf(p)
	}
	if len(ps) > 1 {
		q.push(runtime.MakeGroup(runtime.KOr, uint32(len(ps))))
	}
	return q
}

// Order sets the ordering. It is part of the statement's identity.
func (q Query) Order(ts ...Sort) Query {
	for _, t := range ts {
		q.push(runtime.Tok(t))
	}
	q.sorted = q.sorted || len(ts) > 0
	return q
}

func (q Query) stream() []runtime.Tok { return q.toks[:q.nt] }

func (q Query) preds() bool {
	for _, t := range q.toks[:q.nt] {
		if t.Kind() != runtime.KOrder {
			return true
		}
	}
	return false
}
`, deleted)
	if g.widens() {
		g.p("// WithDeleted reads the marked documents as well as the live ones. All,")
		g.p("// One, Each, Rows, Count and Exists honour it; UpdateAll and DeleteAll")
		g.p("// refuse it, rather than writing to documents every other read calls gone.")
		g.p("func (q Query) WithDeleted() Query { q.deleted = true; return q }")
		g.p("")
		g.p("// scope is 1 for a read that includes marked documents, 0 otherwise.")
		g.p("func (q Query) scope() int {")
		g.p("\tif q.deleted {")
		g.p("\t\treturn 1")
		g.p("\t}")
		g.p("\treturn 0")
		g.p("}")
	} else {
		g.p("func (q Query) scope() int { return 0 }")
	}
	g.p("")
	for _, op := range []string{"Eq", "NotEq"} {
		for i := range g.cols {
			if !g.kind[i].scalar() {
				continue
			}
			g.p("func (q Query) %s%s(v %s) Query { return q.Where(%s.%s(v)) }",
				g.field(i), op, g.kind[i].goType, g.field(i), op)
		}
	}
	g.p("")
}

// widens says the model let call sites read this table's marked rows.
func (g *gen) widens() bool { return g.soft >= 0 && g.t.ReadsDeleted }
