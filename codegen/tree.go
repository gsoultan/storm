package codegen

import "strings"

// Tree-shaped query emission.
//
// Predicates are appended to a postfix token stream rather than folded into a
// per-column operator mask, so `A AND (B OR C)` and `NOT D` are representable.
// Values live in small per-kind arenas indexed in leaf order; bind walks the
// same order, so no index has to be stored in the token.

// arenaFor names the value arena a column's values live in.
func arenaFor(c colInfo) (arena, cursor string) {
	switch c.kind {
	case kindText:
		return "strs", "ns"
	case kindUUID:
		return "raws", "nr"
	case kindTimestamptz:
		return "tims", "ntm"
	case kindFloat4, kindFloat8:
		return "f64s", "nf"
	case kindNumeric:
		// Its own arena: a Decimal is two words and does not fit the int64
		// slot every other scalar shares.
		return "decs", "nd"
	case kindJSONB, kindBytes:
		// No arena. Neither is a value a predicate binds or an ordering
		// compares — jsonb offers only IS [NOT] NULL, and bytea offers
		// nothing — so there is nothing to store.
		return "", ""
	default:
		return "nums", "nn"
	}
}

func arenaCast(c colInfo, expr string) string {
	switch c.kind {
	case kindText, kindUUID, kindTimestamptz, kindNumeric:
		return expr
	case kindFloat4:
		return "float32(" + expr + ")"
	case kindFloat8:
		return expr
	case kindBool:
		return expr + " != 0"
	default:
		return c.goBase + "(" + expr + ")"
	}
}

func arenaStore(c colInfo, v string) string {
	switch c.kind {
	case kindText, kindUUID, kindTimestamptz, kindNumeric:
		return v
	case kindFloat4, kindFloat8:
		return "float64(" + v + ")"
	case kindBool:
		return "b2i(" + v + ")"
	default:
		return "int64(" + v + ")"
	}
}

const (
	maxToks  = 16
	maxOrder = 4
	maxStr   = 6
	maxNum   = 6
	maxRaw   = 4
	maxTime  = 4
	maxFloat = 4
	maxDec   = 4
)

func (g *gen) treeQuery() {
	g.p("// Query is a value type: composing one allocates nothing. Predicates")
	g.p("// are a postfix token stream, so disjunction and negation are")
	g.p("// representable — a per-column operator mask cannot express either.")
	g.p("type Query struct {")
	g.p("\ttoks [%d]runtime.Tok", maxToks)
	g.p("\tnt   uint8")
	g.p("\ttop  uint8 // top-level conjuncts, ANDed at compile time")
	g.p("")
	g.p("\tstrs [%d]string", maxStr)
	g.p("\tnums [%d]int64", maxNum)
	g.p("\traws [%d][16]byte", maxRaw)
	g.p("\ttims [%d]time.Time", maxTime)
	g.p("\tf64s [%d]float64", maxFloat)
	g.p("\tdecs [%d]runtime.Decimal", maxDec)
	g.p("\tns, nn, nr, ntm, nf, nd uint8")
	g.p("")
	g.p("\tanyRaw [][16]byte")
	g.p("\tanyStr []string")
	g.p("\thasAny bool")
	g.p("")
	g.p("\t// Order terms live in their own buffer and are appended to the stream")
	g.p("\t// after the predicate tree. Sharing one buffer would let a Where after")
	g.p("\t// an Order interleave the two, and the stream's order is its meaning.")
	g.p("\totoks [%d]runtime.Tok", maxOrder)
	g.p("\tno    uint8")
	g.p("")
	g.p("\tlimit  int64")
	g.p("\toffset int64")
	g.p("\t// over records that the query outgrew its fixed buffers. Terminals")
	g.p("\t// return it as an error rather than silently dropping a predicate.")
	g.p("\tover bool")
	g.p("")
	g.p("\t// mixed records an After() over an ordering that is not all in one")
	g.p("\t// direction. No single row comparison expresses that.")
	g.p("\tmixed bool")
	g.p("}")
	g.p("")
	g.p("// New starts a query.")
	g.p("func New() Query { return Query{limit: 1000} }")
	g.p("")
	g.p("// Limit caps the result set.")
	g.p("func (q Query) Limit(n int64) Query { q.limit = n; return q }")
	g.p("")
	g.p("// Offset skips rows.")
	g.p("//")
	g.p("// It is here because callers expect it, not because it is a good idea: the")
	g.p("// database still walks and discards every skipped row, so page 5,000 costs")
	g.p("// 5,000 pages of work, and a row inserted mid-scroll shifts every later")
	g.p("// page. Order by a unique key and filter past the last one you saw instead.")
	g.p("func (q Query) Offset(n int64) Query { q.offset = n; return q }")
	g.p("")
	g.p("// After filters past a row already seen: keyset pagination.")
	g.p("//")
	g.p("// The comparison follows the ACTIVE ordering, because it has to. Paging")
	g.p("// `ORDER BY email DESC` with `> $1` walks away from the rows you have")
	g.p("// not seen yet, and silently returns a wrong page rather than an error.")
	g.p("// So After reads the ordering off the query and builds the matching row")
	g.p("// comparison: (a, b) > ($1, $2) for ascending, < for descending.")
	g.p("//")
	g.p("// Unlike Offset the database does not walk the skipped rows — the")
	g.p("// comparison seeks straight into the index — and a row inserted")
	g.p("// mid-scroll cannot shift a page, because the cursor is a position in the")
	g.p("// data rather than a count.")
	g.p("//")
	g.p("// Mixing ascending and descending terms in one ordering is a query error:")
	g.p("// no single row comparison expresses it, and expanding it into ORs would")
	g.p("// silently give up the index walk that makes this worth doing.")
	g.p("func (q Query) After(r Row) Query {")
	g.p("\tterms := q.otoks[:q.no]")
	g.p("\tif q.no == 0 {")
	g.p("\t\tterms = defaultOrder[:]")
	g.p("\t}")
	g.p("\tif len(terms) == 0 {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\tdesc := terms[0].Op() == runtime.Desc || terms[0].Op() == runtime.DescNullsLast")
	g.p("\tfor _, t := range terms {")
	g.p("\t\td := t.Op() == runtime.Desc || t.Op() == runtime.DescNullsLast")
	g.p("\t\tif d != desc {")
	g.p("\t\t\tq.mixed = true")
	g.p("\t\t\treturn q")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tfor _, t := range terms {")
	g.p("\t\tq.push(runtime.MakeCol(t.Col()))")
	g.p("\t\tq.cursor(t.Col(), r)")
	g.p("\t}")
	g.p("\top := runtime.CmpGt")
	g.p("\tif desc {")
	g.p("\t\top = runtime.CmpLt")
	g.p("\t}")
	g.p("\tq.push(runtime.MakeRowCmp(op, uint32(len(terms))))")
	g.p("\tq.top++")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// cursor stores the bound value for one column of a keyset comparison.")
	g.p("func (q *Query) cursor(col uint32, r Row) {")
	g.p("\tswitch col {")
	for i, c := range g.cols {
		arena, cur := arenaFor(c)
		if arena == "" {
			// Not orderable, so it can never be a cursor column. Emitting a
			// case would try to store it in an arena that does not exist.
			continue
		}
		field := exportName(c.Name())
		val := "r." + field
		if isNullable(c.col) {
			val = "r." + field + ".V"
		}
		g.p("\tcase %d:", i)
		g.p("\t\tif int(q.%s) >= len(q.%s) {", cur, arena)
		g.p("\t\t\tq.over = true")
		g.p("\t\t\treturn")
		g.p("\t\t}")
		g.p("\t\tq.%s[q.%s] = %s", arena, cur, arenaStore(c, val))
		g.p("\t\tq.%s++", cur)
	}
	g.p("\t}")
	g.p("}")
	g.p("")
	g.p("// Sort is one ORDER BY term, produced by a column handle: Email.Asc().")
	g.p("type Sort runtime.Tok")
	g.p("")
	g.p("// Order replaces the ordering. Passing none restores the default.")
	g.p("//")
	g.p("// An ordering is part of a statement's identity, not a decoration on it:")
	g.p("// two queries differing only in ORDER BY are different statements, so the")
	g.p("// terms join the token stream that keys the compiled-statement cache.")
	g.p("func (q Query) Order(ts ...Sort) Query {")
	g.p("\tq.no = 0")
	g.p("\tfor _, t := range ts {")
	g.p("\t\tif int(q.no) >= len(q.otoks) {")
	g.p("\t\t\tq.over = true")
	g.p("\t\t\treturn q")
	g.p("\t\t}")
	g.p("\t\tq.otoks[q.no] = runtime.Tok(t)")
	g.p("\t\tq.no++")
	g.p("\t}")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// Err reports a query that outgrew its buffers.")
	g.p("func (q Query) Err() error {")
	g.p("\tif q.over {")
	g.p("\t\treturn errTooComplex")
	g.p("\t}")
	g.p("\tif q.mixed {")
	g.p("\t\treturn errMixedOrder")
	g.p("\t}")
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	g.p("var errMixedOrder = errors.New(")
	g.p("\t%q)", "raorm: After() needs every ORDER BY term in the same direction; "+
		"a mixed ordering has no single row comparison, and expanding it into ORs "+
		"gives up the index walk that makes keyset pagination worth doing")
	g.p("")
	g.p("var errTooComplex = errors.New(")
	g.p("\t%q)", "raorm: query has more predicates than the generated buffers hold; "+
		"split it, or raise the limits in codegen")
	g.p("")
	g.p("// push and leaf mutate through a pointer so the builder loop does not")
	g.p("// copy the whole Query twice per predicate. They are only ever called on")
	g.p("// a local, so the public API keeps value semantics: q2 := q1.Where(x)")
	g.p("// must not mutate q1.")
	g.p("func (q *Query) push(t runtime.Tok) {")
	g.p("\tif int(q.nt) >= len(q.toks) {")
	g.p("\t\tq.over = true")
	g.p("\t\treturn")
	g.p("\t}")
	g.p("\tq.toks[q.nt] = t")
	g.p("\tq.nt++")
	g.p("}")
	g.p("")
	g.p("// preds returns the predicate stream with top-level conjuncts ANDed.")
	g.p("// A count uses this: ordering a scalar is wasted work, and including the")
	g.p("// terms would compile a second statement per ordering for no difference.")
	g.p("func (q Query) preds(buf *[%d]runtime.Tok) []runtime.Tok {", maxToks+maxOrder+1)
	g.p("\tn := copy(buf[:], q.toks[:q.nt])")
	g.p("\tif q.top > 1 {")
	g.p("\t\tbuf[n] = runtime.MakeGroup(runtime.KAnd, uint32(q.top))")
	g.p("\t\tn++")
	g.p("\t}")
	g.p("\treturn buf[:n]")
	g.p("}")
	g.p("")
	g.p("// stream is the predicates followed by the ordering — the whole statement")
	g.p("// key. Order tokens go last so a splicer can find the boundary by kind.")
	g.p("func (q Query) stream(buf *[%d]runtime.Tok) []runtime.Tok {", maxToks+maxOrder+1)
	g.p("\tn := len(q.preds(buf))")
	g.p("\tif q.no == 0 {")
	g.p("\t\tn += copy(buf[n:], defaultOrder[:])")
	g.p("\t\treturn buf[:n]")
	g.p("\t}")
	g.p("\tn += copy(buf[n:], q.otoks[:q.no])")
	g.p("\treturn buf[:n]")
	g.p("}")
	g.p("")
}

// treePreds emits Where / Any / Not plus the typed leaf constructors.
func (g *gen) treePreds() {
	g.p("// Where applies predicates, ANDed together.")
	g.p("func (q Query) Where(ps ...Pred) Query {")
	g.p("\tfor i := range ps {")
	g.p("\t\tq.leaf(ps[i])")
	g.p("\t\tq.top++")
	g.p("\t}")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// WhereIf applies a predicate only when cond holds.")
	g.p("func (q Query) WhereIf(cond bool, p Pred) Query {")
	g.p("\tif !cond {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\treturn q.Where(p)")
	g.p("}")
	g.p("")
	g.p("// Any ORs its arguments and ANDs the group with the rest of the query:")
	g.p("// Where(a).Any(b, c) is a AND (b OR c).")
	g.p("func (q Query) Any(ps ...Pred) Query {")
	g.p("\tif len(ps) == 0 {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\tfor i := range ps {")
	g.p("\t\tq.leaf(ps[i])")
	g.p("\t}")
	g.p("\tq.push(runtime.MakeGroup(runtime.KOr, uint32(len(ps))))")
	g.p("\tq.top++")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// Not negates one predicate and ANDs it with the rest.")
	g.p("func (q Query) Not(p Pred) Query {")
	g.p("\tq.leaf(p)")
	g.p("\tq.push(runtime.MakeGroup(runtime.KNot, 1))")
	g.p("\tq.top++")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// NotAny negates a disjunction: NOT (a OR b).")
	g.p("func (q Query) NotAny(ps ...Pred) Query {")
	g.p("\tif len(ps) == 0 {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\tfor i := range ps {")
	g.p("\t\tq.leaf(ps[i])")
	g.p("\t}")
	g.p("\tq.push(runtime.MakeGroup(runtime.KOr, uint32(len(ps))))")
	g.p("\tq.push(runtime.MakeGroup(runtime.KNot, 1))")
	g.p("\tq.top++")
	g.p("\treturn q")
	g.p("}")
	g.p("")

	// leaf: store the value in its arena, append the token.
	g.p("// leaf records one predicate: its value goes to the arena for its type,")
	g.p("// its structure to the token stream.")
	g.p("func (q *Query) leaf(p Pred) {")
	g.p("\tif p.op == opIn {")
	g.p("\t\tswitch {")
	g.p("\t\tcase p.anyRaw != nil:")
	g.p("\t\t\tq.anyRaw, q.hasAny = p.anyRaw, true")
	g.p("\t\tcase p.anyStr != nil:")
	g.p("\t\t\tq.anyStr, q.hasAny = p.anyStr, true")
	g.p("\t\t}")
	g.p("\t\tq.push(runtime.MakeLeaf(uint32(p.op), uint32(p.col)))")
	g.p("\t\treturn")
	g.p("\t}")
	g.p("\tswitch p.col {")
	for i, c := range g.cols {
		arena, cur := arenaFor(c)
		if arena == "" {
			continue // no value arena: this column has no value-taking operator
		}
		max := map[string]int{"strs": maxStr, "nums": maxNum, "raws": maxRaw,
			"tims": maxTime, "f64s": maxFloat, "decs": maxDec}[arena]
		g.p("\tcase %d:", i)
		g.p("\t\tif int(q.%s) >= %d {", cur, max)
		g.p("\t\t\tq.over = true")
		g.p("\t\t\treturn")
		g.p("\t\t}")
		g.p("\t\tq.%s[q.%s] = %s", arena, cur, predSlotFor(c))
		g.p("\t\tq.%s++", cur)
	}
	g.p("\t}")
	g.p("\tq.push(runtime.MakeLeaf(uint32(p.op), uint32(p.col)))")
	g.p("}")
	g.p("")
}

// predSlotFor reads a Pred's payload into the arena's element type.
func predSlotFor(c colInfo) string {
	switch c.kind {
	case kindText:
		return "p.str"
	case kindUUID:
		return "p.raw"
	case kindTimestamptz:
		return "p.tim"
	case kindFloat4, kindFloat8:
		return "p.f64"
	case kindNumeric:
		return "p.dec"
	default:
		return "p.num"
	}
}

// treeBind walks the token stream and binds arguments in leaf order.
func (g *gen) treeBind() {
	g.p("type binder struct {")
	g.p("\tvals   []any")
	g.p("\tstrs   [%d]string", maxStr)
	g.p("\tnums   [%d]int64", maxNum)
	g.p("\traws   [%d][16]byte", maxRaw)
	g.p("\ttims   [%d]time.Time", maxTime)
	g.p("\tf64s   [%d]float64", maxFloat)
	g.p("\tdecs   [%d]runtime.Decimal", maxDec)
	g.p("\tanyRaw [][16]byte")
	g.p("\tanyStr []string")
	g.p("\tlimit  int64")
	g.p("\toffset int64")
	g.p("}")
	g.p("")
	g.p("var binders = runtime.NewPool(func() *binder {")
	g.p("\treturn &binder{vals: make([]any, 0, %d)}", maxToks+1)
	g.p("})")
	g.p("")
	g.p("// Binder is the pooled argument buffer.")
	g.p("type Binder = binder")
	g.p("")
	g.p("func GetBinder() *Binder  { return binders.Get() }")
	g.p("func PutBinder(b *Binder) { binders.Put(b) }")
	g.p("")
	g.p("// bind copies arena values into the pooled buffer and points the []any")
	g.p("// at the buffer's own fields — boxing a pointer does not allocate.")
	g.p("func (q Query) bind(b *binder) []any {")
	g.p("\tv := b.vals[:0]")
	used := map[string]bool{}
	for _, c := range g.cols {
		a, _ := arenaFor(c)
		if a == "" {
			continue
		}
		used[map[string]string{"strs": "ns", "nums": "nn", "raws": "nr", "tims": "ntm", "f64s": "nf", "decs": "nd"}[a]] = true
	}
	var cursors []string
	for _, n := range []string{"ns", "nn", "nr", "ntm", "nf", "nd"} {
		if used[n] {
			cursors = append(cursors, n)
		}
	}
	if len(cursors) > 0 {
		g.p("\tvar %s uint8", strings.Join(cursors, ", "))
	}
	g.p("\tfor i := uint8(0); i < q.nt; i++ {")
	g.p("\t\tt := q.toks[i]")
	g.p("\t\t// KLeaf binds a predicate's value; KCol binds a keyset cursor's.")
	g.p("\t\t// Both live in the same per-type arenas in stream order, so the")
	g.p("\t\t// column switch below handles them identically. A KCol carries no")
	g.p("\t\t// operator — its op is opNone, which matches no case in the")
	g.p("\t\t// operator switch and falls straight through.")
	g.p("\t\tif k := t.Kind(); k != runtime.KLeaf && k != runtime.KCol {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tswitch runtime.Op(t.Op()) {")
	g.p("\t\tcase opIsNull, opIsNotNull:")
	g.p("\t\t\tcontinue")
	g.p("\t\tcase opIn:")
	g.p("\t\t\tif q.anyRaw != nil {")
	g.p("\t\t\t\tb.anyRaw = q.anyRaw")
	g.p("\t\t\t\tv = append(v, &b.anyRaw)")
	g.p("\t\t\t} else {")
	g.p("\t\t\t\tb.anyStr = q.anyStr")
	g.p("\t\t\t\tv = append(v, &b.anyStr)")
	g.p("\t\t\t}")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tswitch t.Col() {")
	for i, c := range g.cols {
		arena, _ := arenaFor(c)
		if arena == "" {
			continue // no value arena: nothing to copy into the binder
		}
		short := map[string]string{"strs": "ns", "nums": "nn", "raws": "nr",
			"tims": "ntm", "f64s": "nf", "decs": "nd"}[arena]
		g.p("\t\tcase %d:", i)
		g.p("\t\t\tb.%s[%s] = q.%s[%s]", arena, short, arena, short)
		g.p("\t\t\tv = append(v, &b.%s[%s])", arena, short)
		g.p("\t\t\t%s++", short)
	}
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tb.limit = q.limit")
	g.p("\tv = append(v, &b.limit)")
	g.p("\t// LIMIT and OFFSET are bound last, in the order the suffix spells")
	g.p("\t// them. An offset of zero is absent from the statement, so binding it")
	g.p("\t// would leave an argument nothing consumes.")
	g.p("\tif q.offset > 0 {")
	g.p("\t\tb.offset = q.offset")
	g.p("\t\tv = append(v, &b.offset)")
	g.p("\t}")
	g.p("\tb.vals = v")
	g.p("\treturn v")
	g.p("}")
	g.p("")
}
