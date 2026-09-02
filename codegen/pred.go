package codegen

import "fmt"

// Composable predicates.
//
// The chained form (q.EmailEq(v)) is fast but not composable: a predicate
// cannot be returned from a function, stored in a variable, or shared between
// queries. That composability is the whole argument for objects over strings,
// so the generator also emits typed column handles that produce a Pred value.
//
// A Pred is a value type with a union payload. It is transported into the
// Query by a generated switch that writes the correctly-typed field, so the
// Query itself stays small and nothing is boxed.

func (g *gen) predType() {
	g.p("// Pred is one predicate, produced by a typed column handle. A value with")
	g.p("// a small union payload: no interface, nothing to allocate. Structure is")
	g.p("// assembled by Query into a token stream — see Where/Any/Not.")
	// The union carries only the slots this table's columns can produce: a
	// Pred travels BY VALUE through variadic Where, so its size is paid at
	// every call site. Same measured reasoning as the Query arenas.
	ts := slotsFor(g.cols)
	g.p("type Pred struct {")
	g.p("\tcol uint8")
	g.p("\top  runtime.Op")
	for _, sl := range []struct{ name, decl string }{
		{"num", "num int64"}, {"str", "str string"}, {"raw", "raw [16]byte"},
		{"tim", "tim time.Time"}, {"f64", "f64 float64"},
		{"dec", "dec runtime.Decimal"}, {"pfx", "pfx netip.Prefix"},
		{"tod", "tod runtime.TimeOfDay"}, {"bol", "bol bool"},
		{"rng", "rng runtime.TstzRange"},
	} {
		if ts.preds[sl.name] {
			g.p("\t%s", sl.decl)
		}
	}
	for _, slot := range ts.anyList {
		g.p("\t%s", anyPredDecl(slot))
	}
	g.p("}")
	g.p("")
}

// predCtor renders the expression that packs a value into a Pred.
func predCtor(c colInfo, op string, i int) string {
	set := ""
	switch c.kind {
	case kindText:
		set = "str: v"
	case kindUUID:
		set = "raw: v"
	case kindTimestamptz, kindDate:
		set = "tim: v"
	case kindInet:
		set = "pfx: v"
	case kindNumeric:
		set = "dec: v"
	case kindTimeOfDay:
		// Its own field, NOT the shared int64 one it would otherwise land in.
		// A TimeOfDay converts to int64 silently, so the default branch below
		// compiled fine and wrote the value where the arena never looks —
		// every comparison then bound a zero and matched every row. The
		// Pred field, the arena and the slot reader have to name the same
		// place, and nothing but a test against a real database says so.
		set = "tod: v"
	case kindTstzRange:
		set = "rng: v"
	case kindTSVector:
		// The search TERM is a string and rides the text arena; the column it
		// is matched against is the tsvector.
		set = "str: v"
	case kindBool:
		set = "bol: v"
	case kindFloat4, kindFloat8:
		set = "f64: float64(v)"
	default:
		set = "num: int64(v)"
	}
	return fmt.Sprintf("Pred{col: %d, op: op%s, %s}", i, op, set)
}

// colHandles emits one typed handle per column. The handle's *type* decides
// which predicates exist, so an ordering comparison on a uuid or a LIKE on an
// integer is a compile error rather than a runtime surprise.
// sortMethods emits Asc/Desc/AscNullsFirst/DescNullsLast on every column
// handle. Every column is orderable, unlike predicates where the operator has
// to suit the type.
func (g *gen) sortMethods(typeName string, idx int) {
	for _, m := range []struct{ name, dir string }{
		{"Asc", "runtime.Asc"},
		{"Desc", "runtime.Desc"},
		{"AscNullsFirst", "runtime.AscNullsFirst"},
		{"DescNullsLast", "runtime.DescNullsLast"},
	} {
		g.p("func (h %s) %s() Sort { return Sort(runtime.MakeOrder(%s, uint32(h.c))) }",
			typeName, m.name, m.dir)
	}
	g.p("")
	_ = idx
}

func (g *gen) colHandles() {
	if slotsFor(g.cols).hasBool {
		g.p("// b2i lets a bool ride in the numeric slot of a Pred.")
		g.p("func b2i(b bool) int64 {")
		g.p("\tif b {")
		g.p("\t\treturn 1")
		g.p("\t}")
		g.p("\treturn 0")
		g.p("}")
		g.p("")
	}
	g.p("// Typed column handles. The type of the handle is what makes")
	g.p("// Age.Like(...) fail to compile.")
	g.p("var (")
	for i, c := range g.cols {
		g.p("\t%s = %s{%d}", exportName(c.Name()), handleType(c), i)
	}
	g.p(")")
	g.p("")

	// One handle type per kind actually used, so the generated file stays small.
	seen := map[string]bool{}
	for i, c := range g.cols {
		ht := handleType(c)
		if seen[ht] {
			continue
		}
		seen[ht] = true
		g.p("// %s addresses a %s column.", ht, c.col.Type.SQL())
		g.p("type %s struct{ c uint8 }", ht)
		g.p("")
		g.sortMethods(ht, i)
		for _, op := range ops {
			if !opApplies(op.name, c.kind, c.col) {
				continue
			}
			if op.list() {
				slot := predArraySlot(c)
				if slot == "" {
					continue
				}
				switch op.name {
				case "NotIn":
					g.p("// NotIn is `<> ALL($1)`. A NULL anywhere in v makes the")
					g.p("// comparison NULL for every row and the result empty —")
					g.p("// PostgreSQL's rule for NOT IN, not storm's.")
				case "ArrayContains":
					g.p("// Contains is `@>`: every element of v is in the column.")
					g.p("// An empty v is true for every row — every array contains")
					g.p("// the empty one.")
				case "ArrayContainedBy":
					g.p("// ContainedBy is `<@`: every element of the column is in v.")
				case "ArrayOverlaps":
					g.p("// Overlaps is `&&`: the column and v share an element.")
				}
				g.p("func (h %s) %s(v ...%s) Pred { return Pred{col: h.c, op: op%s, %s: v} }",
					ht, op.m(), listElem(c), op.name, slot)
				continue
			}
			if op.args == 0 {
				g.p("func (h %s) %s() Pred { return Pred{col: h.c, op: op%s} }", ht, op.m(), op.name)
				continue
			}
			ctor := predCtor(c, op.name, i)
			ctor = replaceColField(ctor)
			g.p("func (h %s) %s(v %s) Pred { return %s }", ht, op.m(), c.goBase, ctor)
		}
		g.p("")
	}
}

// replaceColField swaps the literal column index for the handle's own, so one
// handle type serves every column of that kind.
func replaceColField(ctor string) string {
	i := 0
	for ; i < len(ctor); i++ {
		if ctor[i] == ':' {
			break
		}
	}
	j := i
	for ; j < len(ctor); j++ {
		if ctor[j] == ',' {
			break
		}
	}
	return ctor[:i+2] + "h.c" + ctor[j:]
}

// predArraySlot picks the union member a list travels in. Numeric lists are
// left to the chained form for now rather than carrying three more slots.
func predArraySlot(c colInfo) string {
	switch c.kind {
	case kindUUID, kindUUIDArray:
		return "anyRaw"
	case kindText, kindTextArray:
		return "anyStr"
	case kindInt2:
		return "anyI16"
	case kindInt4:
		return "anyI32"
	case kindInt8, kindInt8Array:
		return "anyI64"
	case kindDecimalArray:
		return "anyDec"
	}
	return ""
}

// listElem is the ELEMENT type a column's list operator takes.
//
// For a scalar column it is the column's own type: In on a text column takes
// strings. For an array column it is the array's element type, because the
// argument to `tags @> $1` is another text[] — so the variadic form takes
// strings there too, and both land in the same list slot. That is why an array
// column needs no slot of its own.
func listElem(c colInfo) string {
	for _, sl := range anySlotTable {
		if sl.name == predArraySlot(c) {
			return sl.elem
		}
	}
	return ""
}

// handleType names the handle for a column's kind, with a nullable variant so
// IsNull is only offered where it is meaningful.
func handleType(c colInfo) string {
	base := map[kind]string{
		kindBool:         "BoolCol",
		kindInt2:         "Int16Col",
		kindInt4:         "Int32Col",
		kindInt8:         "Int64Col",
		kindFloat4:       "Float32Col",
		kindFloat8:       "Float64Col",
		kindText:         "TextCol",
		kindTSVector:     "TSVectorCol",
		kindTstzRange:    "TstzRangeCol",
		kindBytes:        "BytesCol",
		kindUUID:         "UUIDCol",
		kindTimestamptz:  "TimeCol",
		kindDate:         "DateCol",
		kindInterval:     "IntervalCol",
		kindTimeOfDay:    "TimeOfDayCol",
		kindInet:         "InetCol",
		kindInt8Array:    "Int64ArrayCol",
		kindNumeric:      "DecimalCol",
		kindJSONB:        "JSONCol",
		kindTextArray:    "TextArrayCol",
		kindUUIDArray:    "UUIDArrayCol",
		kindDecimalArray: "DecimalArrayCol",
	}[c.kind]
	// Every supported kind MUST appear above. A missing entry emitted
	// `Splits = {14}` — a column handle with no type — which the Go parser
	// caught, but only after the generator had already written the file. This
	// is the third per-kind map to be missed while adding a type; each one
	// now says so in its own terms rather than producing nonsense.
	if base == "" {
		panic(fmt.Sprintf("codegen: kind %d has no column-handle type name — add it to the map in colHandleType", c.kind))
	}
	if !c.col.NotNull {
		return "Null" + base
	}
	return base
}
