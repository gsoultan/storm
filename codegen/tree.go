package codegen

import (
	"fmt"
	"strconv"
	"strings"
)

// Tree-shaped query emission.
//
// Predicates are appended to a postfix token stream rather than folded into a
// per-column operator mask, so `A AND (B OR C)` and `NOT D` are representable.
// Values live in small per-kind arenas indexed in leaf order; bind walks the
// same order, so no index has to be stored in the token.

// arenaTable is the ONE definition of a value arena: its field name, its
// cursor variable, its declaration and its capacity.
//
// It exists because this used to be four hand-written lists — the binder
// fields, the Query fields, the cursor declarations and the bind switch — that
// had to agree. Adding `bools` meant editing all four, missing one produced a
// generated file referencing an undeclared cursor, and the version of this bug
// that shipped bound every bool predicate as *int64. One list cannot disagree
// with itself.
var arenaTable = []struct {
	name   string // arena field: "strs"
	cursor string // cursor variable: "ns"
	decl   string // field declaration, with a %d for the capacity
	max    int
}{
	{"strs", "ns", "strs [%d]string", maxStr},
	{"nums", "nn", "nums [%d]int64", maxNum},
	{"raws", "nr", "raws [%d][16]byte", maxRaw},
	{"tims", "ntm", "tims [%d]time.Time", maxTime},
	{"f64s", "nf", "f64s [%d]float64", maxFloat},
	{"decs", "nd", "decs [%d]runtime.Decimal", maxDec},
	{"pfxs", "npf", "pfxs [%d]netip.Prefix", maxPfx},
	{"tods", "nto", "tods [%d]runtime.TimeOfDay", maxTod},
	{"bools", "nbo", "bools [%d]bool", maxBool},
	{"rngs", "nrg", "rngs [%d]runtime.TstzRange", maxRng},
	{"jsns", "njs", "jsns [%d]runtime.JSON", maxJSON},
}

// cursorFor is the cursor variable for an arena field name.
func cursorFor(arena string) string {
	for _, a := range arenaTable {
		if a.name == arena {
			return a.cursor
		}
	}
	return ""
}

// arenaFor names the value arena a column's values live in.
// tableSlots is which value-carrying machinery this table's columns can ever
// touch. Everything downstream — the Query arenas, the binder, the Pred union
// — is emitted from this, and ONLY this.
//
// It exists because the alternative was measured, not imagined: emitting every
// arena unconditionally grew Query to 704 bytes (recorded history: ~150 with
// the mask, ~330 with the tree), with [4]Decimal and [4]netip.Prefix copied on
// every builder call of every table INCLUDING tables with no numeric or inet
// column anywhere. The bench table pays for machinery it cannot name, and the
// warm build+prepare path regressed from 257ns to ~455ns. A value type's size
// is part of its API.
type tableSlots struct {
	arenas  map[string]bool // "strs", "nums", ...
	cursors []string        // "ns", "nn", ... in canonical order
	preds   map[string]bool // "str", "num", ... Pred union members
	// anyList is the list slots this table's columns need, in anySlotTable
	// order. A list rather than one bool per slot: the leaf and bind switches
	// used to hand-enumerate the COMBINATIONS present, which is 2^n cases and
	// was already wrong for a third slot before one existed.
	anyList []string
	hasBool bool // b2i is only needed if a bool column exists
}

// anySlotTable is every list slot a Pred can carry, in a fixed order so
// generated code is byte-identical across runs.
//
// One slot per element type rather than one widened []int64 for all three
// integer widths. Converting int16 to int64 to bind it would allocate a second
// slice per call, and would hand PostgreSQL an int8[] to compare against an
// int2 column — a cast the planner has to undo before it can use an index,
// which is the opposite of what `= ANY` is for.
var anySlotTable = []struct{ name, cursor, elem string }{
	{"anyRaw", "nar", "[16]byte"},
	{"anyStr", "nas", "string"},
	{"anyI16", "nai16", "int16"},
	{"anyI32", "nai32", "int32"},
	{"anyI64", "nai64", "int64"},
	{"anyDec", "nadec", "runtime.Decimal"},
}

// anyCursor is a list slot's cursor variable.
func anyCursor(slot string) string {
	for _, sl := range anySlotTable {
		if sl.name == slot {
			return sl.cursor
		}
	}
	return ""
}

// anyCursors is the cursor variable for each list slot this table needs, in
// anySlotTable order.
func (ts tableSlots) anyCursors() []string {
	var out []string
	for _, slot := range ts.anyList {
		out = append(out, anyCursor(slot))
	}
	return out
}

// has reports whether this table needs the named list slot.
func (ts tableSlots) has(slot string) bool {
	for _, s := range ts.anyList {
		if s == slot {
			return true
		}
	}
	return false
}

func slotsFor(cols []colInfo) tableSlots {
	ts := tableSlots{arenas: map[string]bool{}, preds: map[string]bool{}}
	for _, c := range cols {
		if a, _ := arenaFor(c); a != "" {
			ts.arenas[a] = true
		}
		if p := predSlotFor(c); p != "" {
			ts.preds[strings.TrimPrefix(p, "p.")] = true
		}
		if slot := predArraySlot(c); slot != "" && !ts.has(slot) {
			ts.anyList = append(ts.anyList, slot)
		}
		if c.kind == kindBool {
			ts.hasBool = true
		}
	}
	for _, a := range arenaTable {
		if ts.arenas[a.name] {
			ts.cursors = append(ts.cursors, a.cursor)
		}
	}
	// Re-ordered into anySlotTable order: the loop above walks columns, whose
	// order is the model's, and generated output must not depend on it.
	var ordered []string
	for _, sl := range anySlotTable {
		if ts.has(sl.name) {
			ordered = append(ordered, sl.name)
		}
	}
	ts.anyList = ordered
	return ts
}

// listOpTest and listOpCases name every operator whose argument is a list, in
// ops order. Spelling them out at each site is how In and NotIn diverged from
// the array forms the first time.
func listOpTest(v string) string {
	var b strings.Builder
	for _, op := range ops {
		if !op.list() {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" || ")
		}
		b.WriteString(v + " == op" + op.name)
	}
	return b.String()
}

func listOpCases() string {
	var names []string
	for _, op := range ops {
		if op.list() {
			names = append(names, "op"+op.name)
		}
	}
	return strings.Join(names, ", ")
}

// anyPredDecl is a list slot's field in a PRED, which holds exactly one
// predicate and therefore exactly one list. Only Query and the binder are
// arenas — a Pred that grew to [3] would pay for the arena on every predicate
// in the program, and Pred is copied by value into every builder call.
func anyPredDecl(slot string) string {
	for _, sl := range anySlotTable {
		if sl.name == slot {
			return fmt.Sprintf("%s []%s", sl.name, sl.elem)
		}
	}
	return ""
}

// anyDecl is a list slot's field declaration: an ARENA of lists, indexed by
// the slot's cursor in token order, exactly like the scalar arenas above it.
func anyDecl(slot string, n int) string {
	for _, sl := range anySlotTable {
		if sl.name == slot {
			return fmt.Sprintf("%s [%d][]%s", sl.name, n, sl.elem)
		}
	}
	return ""
}

func arenaFor(c colInfo) (arena, cursor string) {
	switch c.kind {
	case kindText, kindTSVector:
		return "strs", "ns"
	case kindUUID:
		return "raws", "nr"
	case kindTimestamptz, kindDate:
		return "tims", "ntm"
	case kindInet:
		return "pfxs", "npf"
	case kindFloat4, kindFloat8:
		return "f64s", "nf"
	case kindNumeric:
		// Its own arena: a Decimal is two words and does not fit the int64
		// slot every other scalar shares.
		return "decs", "nd"
	case kindTimeOfDay:
		// A TimeOfDay IS an int64 and would fit the shared slot — but the
		// value reaches pgx from the arena, and an int64 there encodes as
		// int8, which is the wrong wire type for a `time` column. Its own
		// arena keeps the Go type intact all the way to the codec.
		return "tods", "nto"
	case kindTstzRange:
		// Its own arena for the Decimal reason: a range is six fields and does
		// not fit the int64 slot every other scalar shares.
		return "rngs", "nrg"
	case kindBool:
		// Exactly the TimeOfDay problem, one type over. A bool packed into the
		// shared int64 arena reaches pgx as *int64, which has no encode plan
		// for OID 16, so EVERY predicate on a bool column failed at execution:
		// "cannot find encode plan". Nothing caught it because no fixture in
		// this repository had a bool column until examples/orders.
		return "bools", "nbo"
	case kindJSONB:
		// Its own arena, holding the ARGUMENT to @> and <@ — not the column's
		// value, which is never bound. A jsonb argument is []byte and would
		// fit nothing else: the shared int64 slot cannot hold it, and the
		// string arena would reach pgx as text, which has no implicit cast to
		// jsonb in an operator position.
		return "jsns", "njs"
	case kindBytes, kindTextArray, kindUUIDArray, kindInt8Array,
		kindDecimalArray, kindInterval:
		// No arena. None is a value a predicate binds or an ordering compares
		// — an array's operators take LISTS, which live in list slots, and
		// bytea offers no predicates at all — so there is nothing to store.
		return "", ""
	default:
		return "nums", "nn"
	}
}

func arenaStore(c colInfo, v string) string {
	switch c.kind {
	case kindText, kindTSVector, kindUUID, kindTimestamptz, kindDate, kindNumeric,
		kindInet, kindTimeOfDay, kindBool, kindTstzRange, kindJSONB:
		return v
	case kindFloat4, kindFloat8:
		return "float64(" + v + ")"
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
	maxTod   = 4
	maxBool  = 4
	maxRng   = 4
	maxPfx   = 4
	maxJSON  = 2
	// List values are an arena like every other value type, for the reason
	// every other value type is one: a Query can carry more than one predicate
	// on the same slot. It used to be a single field, so a second In on a text
	// column OVERWROTE the first and the statement bound one list twice —
	// wrong rows, no error, on every version through v0.3.0.
	//
	// Smaller than the scalar arenas because a list predicate is rarer, and
	// bounded for the same reason they are: past the bound q.over is set and
	// the query errors, which is the difference between a limit and a lie.
	maxAny = 3
)

// itoaBudget renders the scale for the overflow message, so the error names
// the setting it was generated with rather than a knob whose value the reader
// then has to go and find.
func itoaBudget(scale int) string {
	if scale < 1 {
		scale = 1
	}
	return strconv.Itoa(scale)
}

// budget scales one of the constants above by Options.Budgets.Scale.
//
// The numbers are relative to each other by measurement — a text predicate is
// commoner than a jsonb one, a list predicate rarer than a scalar — so one
// factor moves them together and keeps the shape the measurement found. It is
// applied here, in the one place a buffer size is decided, so no emitter can
// size a buffer the bind loop disagrees with.
// streamBuf is the scratch a caller passes to preds/stream: every predicate
// token, every order token, and the one group token that ANDs the top level.
func (g *gen) streamBuf() int { return g.budget(maxToks) + g.budget(maxOrder) + 1 }

// composedBuf is the scratch a COMPOSED statement needs: a parent's stream and
// a wrapped child's, plus the group tokens that join them.
func (g *gen) composedBuf() int { return 2*(g.budget(maxToks)+g.budget(maxOrder)) + 4 }

func (g *gen) budget(n int) int {
	s := g.o.Budgets.Scale
	if s < 1 {
		s = 1
	}
	return n * s
}

func (g *gen) treeQuery() {
	g.p("// Query is a value type: composing one allocates nothing. Predicates")
	g.p("// are a postfix token stream, so disjunction and negation are")
	g.p("// representable — a per-column operator mask cannot express either.")
	g.p("type Query struct {")
	g.p("\ttoks [%d]runtime.Tok", g.budget(maxToks))
	g.p("\tnt   uint8")
	g.p("\ttop  uint8 // top-level conjuncts, ANDed at compile time")
	g.p("")
	ts := slotsFor(g.cols)
	// Arenas exist per KIND THE TABLE HAS, not per kind storm supports. A
	// value type's size is part of its API: every builder call copies Query,
	// and a table with no inet column must not carry four netip.Prefix slots
	// on every copy. Measured, not aesthetic — see slotsFor.
	for _, ar := range arenaTable {
		if ts.arenas[ar.name] {
			g.p("\t"+ar.decl, g.budget(ar.max))
		}
	}
	g.p("\t%s uint8", strings.Join(ts.cursors, ", "))
	g.p("")
	for _, slot := range ts.anyList {
		g.p("\t%s", anyDecl(slot, g.budget(maxAny)))
	}
	if cs := ts.anyCursors(); len(cs) > 0 {
		g.p("\t%s uint8", strings.Join(cs, ", "))
	}
	g.p("")
	g.p("\t// Order terms live in their own buffer and are appended to the stream")
	g.p("\t// after the predicate tree. Sharing one buffer would let a Where after")
	g.p("\t// an Order interleave the two, and the stream's order is its meaning.")
	g.p("\totoks [%d]runtime.Tok", g.budget(maxOrder))
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
	g.p("")
	g.p("\t// noOrder suppresses the default ordering. See Unordered.")
	g.p("\tnoOrder bool")
	g.p("\tbadAfter bool")
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
	g.p("\t\tif q.noOrder {")
	g.p("\t\t\t// A keyset cursor without an order is a position in nothing.")
	g.p("\t\t\tq.badAfter = true")
	g.p("\t\t\treturn q")
	g.p("\t\t}")
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
		if !readable(c.col) {
			// A keyset cursor reads its value off a Row, and a tsvector is not
			// in one. Emitting a case would reference a field that does not
			// exist — and paging by a search vector is not a thing anyone
			// means anyway.
			continue
		}
		if c.kind == kindJSONB {
			// jsonb has an arena, but it holds the ARGUMENT to @> and <@, not
			// a value anyone pages by. PostgreSQL does define a total order on
			// jsonb; nobody means it, and a cursor case here would spend the
			// argument arena on ordering.
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
	g.p("// Unordered drops the default ordering, for results consumed in an")
	g.p("// order-independent way — a batch loader that buckets rows into a map,")
	g.p("// an aggregation, a bulk export.")
	g.p("//")
	g.p("// The default exists because paging an unordered read is a bug; the")
	g.p("// escape exists because ordering a read NOBODY PAGES is a server-side")
	g.p("// sort for free. Measured on a 50k-row relation load: an external merge")
	g.p("// sort spilling 5MB to disk, for an order the loader's map bucketing")
	g.p("// destroyed on arrival. An explicit Order() still applies; After() on an")
	g.p("// unordered query with no explicit order is an error, because a keyset")
	g.p("// cursor without an order is a position in nothing.")
	g.p("func (q Query) Unordered() Query { q.noOrder = true; return q }")
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
	g.p("\tif q.badAfter {")
	g.p("\t\treturn errAfterUnordered")
	g.p("\t}")
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	g.p("var errAfterUnordered = errors.New(")
	g.p("\t%q)", "storm: After() on an Unordered() query with no explicit Order — "+
		"a keyset cursor without an ordering is a position in nothing")
	g.p("")
	g.p("var errMixedOrder = errors.New(")
	g.p("\t%q)", "storm: After() needs every ORDER BY term in the same direction; "+
		"a mixed ordering has no single row comparison, and expanding it into ORs "+
		"gives up the index walk that makes keyset pagination worth doing")
	g.p("")
	g.p("var errTooComplex = errors.New(")
	g.p("\t%q)", "storm: query has more predicates than the generated buffers hold "+
		"(scale "+itoaBudget(g.o.Budgets.Scale)+
		"); split the query, or regenerate with a larger codegen.Budgets{Scale}")
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
	g.p("func (q Query) preds(buf *[%d]runtime.Tok) []runtime.Tok {", g.streamBuf())
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
	g.p("func (q Query) stream(buf *[%d]runtime.Tok) []runtime.Tok {", g.streamBuf())
	g.p("\tn := len(q.preds(buf))")
	g.p("\tif q.no == 0 {")
	g.p("\t\tif q.noOrder {")
	g.p("\t\t\treturn buf[:n]")
	g.p("\t\t}")
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
	ts := slotsFor(g.cols)
	_ = ts
	g.p("// Where applies predicates, ANDed together.")
	g.p("func (q Query) Where(ps ...Pred) Query {")
	g.p("\tfor i := range ps {")
	g.p("\t\tq.leaf(ps[i])")
	g.p("\t\tq.top++")
	g.p("\t}")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// WhenSet applies f(*v) only when v is non-nil — the optional-filter")
	g.p("// idiom without an if, and without the nil dereference WhereIf invites.")
	g.p("//")
	g.p("//\tq = user.WhenSet(q, f.MinAge, user.Age.Gte)")
	g.p("//")
	g.p("// WhereIf takes an already-built Pred, so the caller has to evaluate")
	g.p("// user.Age.Gte(*f.MinAge) BEFORE the condition is tested — which panics")
	g.p("// on exactly the nil the condition was checking for. This takes the")
	g.p("// constructor instead, so nothing is dereferenced unless it is there.")
	g.p("//")
	g.p("// It still sets exactly one bit in the shape mask: a filter that is")
	g.p("// absent is a different SHAPE, compiled once, not a different value.")
	g.p("func WhenSet[T any](q Query, v *T, f func(T) Pred) Query {")
	g.p("\tif v == nil {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\treturn q.Where(f(*v))")
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
	g.p("// Grp is one conjunction, built by And, that AnyOf ORs with the others.")
	g.p("//")
	g.p("// It holds the predicates rather than tokens because the ARENA a value")
	g.p("// lands in is chosen by leaf, in stream order, and a group has no stream")
	g.p("// position until AnyOf gives it one.")
	g.p("type Grp struct{ ps []Pred }")
	g.p("")
	g.p("// And groups predicates into one conjunction, so AnyOf can OR whole")
	g.p("// conjunctions rather than single predicates:")
	g.p("//")
	g.p("//\tq.AnyOf(And(Status.Eq(\"paid\"), Total.Gt(big)),")
	g.p("//\t        And(Status.Eq(\"trial\"), Total.Gt(small)))")
	g.p("//")
	g.p("// is (status = $1 AND total > $2) OR (status = $3 AND total > $4). Any")
	g.p("// ORs single predicates and cannot say this: an advanced-search screen")
	g.p("// whose rows are each a field, an operator and a value produces exactly")
	g.p("// this shape, and without it the query has to be written in SQL.")
	g.p("//")
	g.p("// The variadic slice does not escape — AnyOf reads it and returns — so a")
	g.p("// warm call still builds its SQL without allocating.")
	g.p("func And(ps ...Pred) Grp { return Grp{ps: ps} }")
	g.p("")
	g.p("// AnyOf ORs its groups and ANDs the result with the rest of the query.")
	g.p("//")
	g.p("// An empty group contributes nothing, so a screen that builds one group")
	g.p("// per filled-in filter row needs no special case for the rows the user")
	g.p("// left blank. A group of one predicate is that predicate: the SQL carries")
	g.p("// no parentheses it did not need, which keeps the statement — and so the")
	g.p("// shape it is cached under — the same as the equivalent Any.")
	g.p("func (q Query) AnyOf(gs ...Grp) Query {")
	g.p("\tn := uint32(0)")
	g.p("\tfor i := range gs {")
	g.p("\t\tif len(gs[i].ps) == 0 {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tfor j := range gs[i].ps {")
	g.p("\t\t\tq.leaf(gs[i].ps[j])")
	g.p("\t\t}")
	g.p("\t\tif len(gs[i].ps) > 1 {")
	g.p("\t\t\tq.push(runtime.MakeGroup(runtime.KAnd, uint32(len(gs[i].ps))))")
	g.p("\t\t}")
	g.p("\t\tn++")
	g.p("\t}")
	g.p("\tif n == 0 {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\tif n > 1 {")
	g.p("\t\tq.push(runtime.MakeGroup(runtime.KOr, n))")
	g.p("\t}")
	g.p("\tq.top++")
	g.p("\treturn q")
	g.p("}")
	g.p("")
	g.p("// NotAnyOf negates the disjunction AnyOf builds: NOT ((a AND b) OR c).")
	g.p("func (q Query) NotAnyOf(gs ...Grp) Query {")
	g.p("\tbefore := q.top")
	g.p("\tq = q.AnyOf(gs...)")
	g.p("\tif q.top == before {")
	g.p("\t\treturn q")
	g.p("\t}")
	g.p("\tq.push(runtime.MakeGroup(runtime.KNot, 1))")
	g.p("\treturn q")
	g.p("}")
	g.p("")

	// leaf: store the value in its arena, append the token.
	g.p("// leaf records one predicate: its value goes to the arena for its type,")
	g.p("// its structure to the token stream.")
	g.p("func (q *Query) leaf(p Pred) {")
	// Only the list branches this table's columns can produce. The generic
	// two-branch switch referenced slots the trimmed Pred no longer carries.
	// In and NotIn share every branch: both bind ONE list to ONE placeholder,
	// and only the operator text between them differs.
	if len(ts.anyList) > 0 {
		// Dispatch on the COLUMN, not on which Pred field is non-nil. The
		// value goes to the arena for its type at that arena's cursor, which
		// is what lets a query carry more than one list predicate — the single
		// field this replaced made the second one overwrite the first.
		g.p("\tif %s {", listOpTest("p.op"))
		g.p("\t\tswitch p.col {")
		for i, c := range g.cols {
			slot := predArraySlot(c)
			if slot == "" {
				continue
			}
			cur := anyCursor(slot)
			g.p("\t\tcase %d:", i)
			g.p("\t\t\tif int(q.%s) >= %d {", cur, g.budget(maxAny))
			g.p("\t\t\t\tq.over = true")
			g.p("\t\t\t\treturn")
			g.p("\t\t\t}")
			g.p("\t\t\tq.%s[q.%s] = p.%s", slot, cur, slot)
			g.p("\t\t\tq.%s++", cur)
		}
		g.p("\t\t}")
		g.p("\t\tq.push(runtime.MakeLeaf(uint32(p.op), uint32(p.col)))")
		g.p("\t\treturn")
		g.p("\t}")
	}
	g.p("\tswitch p.col {")
	for i, c := range g.cols {
		arena, cur := arenaFor(c)
		if arena == "" {
			continue // no value arena: this column has no value-taking operator
		}
		// Every arena MUST appear here. A missing entry reads as capacity
		// zero, so the first predicate on that column overflows and the query
		// fails as "too complex" — which is what an inet column did once, and
		// a time column did again while this type was being added.
		max := 0
		for _, a := range arenaTable {
			if a.name == arena {
				max = a.max
			}
		}
		if max == 0 {
			g.err = fmt.Errorf("codegen: arena %q has no capacity entry — add it to the map in treePreds", arena)
			return
		}
		g.p("\tcase %d:", i)
		g.p("\t\tif int(q.%s) >= %d {", cur, g.budget(max))
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
	case kindText, kindTSVector:
		return "p.str"
	case kindUUID:
		return "p.raw"
	case kindTimestamptz, kindDate:
		return "p.tim"
	case kindFloat4, kindFloat8:
		return "p.f64"
	case kindNumeric:
		return "p.dec"
	case kindTimeOfDay:
		return "p.tod"
	case kindTstzRange:
		return "p.rng"
	case kindBool:
		return "p.bol"
	case kindInet:
		return "p.pfx"
	case kindJSONB:
		return "p.jsn"
	default:
		return "p.num"
	}
}

// treeBind walks the token stream and binds arguments in leaf order.
func (g *gen) treeBind() {
	ts := slotsFor(g.cols)
	g.p("type binder struct {")
	g.p("\tvals   []any")
	for _, ar := range arenaTable {
		if ts.arenas[ar.name] {
			g.p("\t"+ar.decl, g.budget(ar.max))
		}
	}
	for _, slot := range ts.anyList {
		g.p("\t%s", anyDecl(slot, g.budget(maxAny)))
	}
	g.p("\tlimit  int64")
	g.p("\toffset int64")
	g.p("}")
	g.p("")
	g.p("var binders = runtime.NewPool(func() *binder {")
	g.p("\treturn &binder{vals: make([]any, 0, %d)}", g.budget(maxToks)+1)
	g.p("})")
	g.p("")
	g.p("// Binder is the pooled argument buffer.")
	g.p("type Binder = binder")
	g.p("")
	g.p("func GetBinder() *Binder  { return binders.Get() }")
	g.p("func PutBinder(b *Binder) { putBinder(b) }")
	g.p("")
	g.p("// putBinder clears every field that references memory OUTSIDE the binder")
	g.p("// before pooling it. A pooled binder otherwise PINS the caller's data —")
	g.p("// the slice behind an In(...) and the bytes behind every bound string —")
	g.p("// for as long as the pool holds it, which is forever. The numeric, time")
	g.p("// and uuid arenas are value arrays and pin nothing; vals points at the")
	g.p("// binder's own fields. A few nil stores against a round trip is free.")
	g.p("func putBinder(b *binder) {")
	if ts.arenas["strs"] {
		g.p("\tfor i := range b.strs {")
		g.p("\t\tb.strs[i] = \"\"")
		g.p("\t}")
	}
	for _, slot := range ts.anyList {
		g.p("\tfor i := range b.%s {", slot)
		g.p("\t\tb.%s[i] = nil", slot)
		g.p("\t}")
	}
	g.p("\tbinders.Put(b)")
	g.p("}")
	g.p("")
	g.p("// bindPreds copies arena values into the pooled buffer and points the")
	g.p("// []any at the buffer's own fields — boxing a pointer does not allocate.")
	g.p("// Count and Exists stop here: their statements carry no LIMIT or OFFSET.")
	g.p("func (q Query) bindPreds(b *binder) []any {")
	g.p("\tv := b.vals[:0]")
	used := map[string]bool{}
	for _, c := range g.cols {
		a, _ := arenaFor(c)
		if a == "" {
			continue
		}
		used[cursorFor(a)] = true
	}
	var cursors []string
	for _, a := range arenaTable {
		if used[a.cursor] {
			cursors = append(cursors, a.cursor)
		}
	}
	cursors = append(cursors, ts.anyCursors()...)
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
	if len(ts.anyList) > 0 {
		g.p("\t\tcase %s:", listOpCases())
		g.p("\t\t\tswitch t.Col() {")
		for i, c := range g.cols {
			slot := predArraySlot(c)
			if slot == "" {
				continue
			}
			cur := anyCursor(slot)
			g.p("\t\t\tcase %d:", i)
			g.p("\t\t\t\tb.%s[%s] = q.%s[%s]", slot, cur, slot, cur)
			g.p("\t\t\t\tv = append(v, &b.%s[%s])", slot, cur)
			g.p("\t\t\t\t%s++", cur)
		}
		g.p("\t\t\t}")
		g.p("\t\t\tcontinue")
	}
	g.p("\t\t}")
	g.p("\t\tswitch t.Col() {")
	for i, c := range g.cols {
		arena, _ := arenaFor(c)
		if arena == "" {
			continue // no value arena: nothing to copy into the binder
		}
		short := cursorFor(arena)
		g.p("\t\tcase %d:", i)
		g.p("\t\t\tb.%s[%s] = q.%s[%s]", arena, short, arena, short)
		g.p("\t\t\tv = append(v, &b.%s[%s])", arena, short)
		g.p("\t\t\t%s++", short)
	}
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tb.vals = v")
	g.p("\treturn v")
	g.p("}")
	g.p("")
	g.p("// bind is bindPreds plus the paging arguments, in suffix order.")
	g.p("func (q Query) bind(b *binder) []any {")
	g.p("\tv := q.bindPreds(b)")
	g.p("\tb.limit = q.limit")
	g.p("\tv = append(v, &b.limit)")
	g.p("\t// An offset of zero is absent from the statement, so binding it")
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
