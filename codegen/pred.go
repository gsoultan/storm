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
	g.p("type Pred struct {")
	g.p("\tcol uint8")
	g.p("\top  runtime.Op")
	g.p("\tnum int64")
	g.p("\tstr string")
	g.p("\traw [16]byte")
	g.p("\ttim time.Time")
	g.p("\tf64 float64")
	g.p("\tdec runtime.Decimal")
	g.p("\tpfx netip.Prefix")
	g.p("\tanyRaw [][16]byte")
	g.p("\tanyStr []string")
	g.p("}")
	g.p("")
}

// predField picks the union member a column's value travels in.
func predField(c colInfo) string {
	switch c.kind {
	case kindText:
		return "p.str"
	case kindUUID:
		return "p.raw"
	case kindTimestamptz, kindDate:
		return "p.tim"
	case kindInet:
		return "p.pfx"
	case kindBool:
		return "p.num != 0"
	case kindFloat4:
		return "float32(p.f64)"
	case kindFloat8:
		return "p.f64"
	case kindInt2:
		return "int16(p.num)"
	case kindInt4:
		return "int32(p.num)"
	case kindInt8:
		return "p.num"
	case kindNumeric:
		return "p.dec"
	case kindBytes:
		return "nil"
	}
	return "nil"
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
	case kindBool:
		set = "num: b2i(v)"
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
	g.p("// b2i lets a bool ride in the numeric slot of a Pred.")
	g.p("func b2i(b bool) int64 {")
	g.p("\tif b {")
	g.p("\t\treturn 1")
	g.p("\t}")
	g.p("\treturn 0")
	g.p("}")
	g.p("")
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
			if op.name == "In" {
				if slot := predArraySlot(c); slot != "" {
					g.p("func (h %s) In(v ...%s) Pred { return Pred{col: h.c, op: opIn, %s: v} }",
						ht, c.goBase, slot)
				}
				continue
			}
			if op.args == 0 {
				g.p("func (h %s) %s() Pred { return Pred{col: h.c, op: op%s} }", ht, op.name, op.name)
				continue
			}
			ctor := predCtor(c, op.name, i)
			ctor = replaceColField(ctor)
			g.p("func (h %s) %s(v %s) Pred { return %s }", ht, op.name, c.goBase, ctor)
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
	case kindUUID:
		return "anyRaw"
	case kindText:
		return "anyStr"
	}
	return ""
}

// handleType names the handle for a column's kind, with a nullable variant so
// IsNull is only offered where it is meaningful.
func handleType(c colInfo) string {
	base := map[kind]string{
		kindBool:        "BoolCol",
		kindInt2:        "Int16Col",
		kindInt4:        "Int32Col",
		kindInt8:        "Int64Col",
		kindFloat4:      "Float32Col",
		kindFloat8:      "Float64Col",
		kindText:        "TextCol",
		kindBytes:       "BytesCol",
		kindUUID:        "UUIDCol",
		kindTimestamptz: "TimeCol",
		kindDate:        "DateCol",
		kindInterval:    "IntervalCol",
		kindInet:        "InetCol",
		kindInt8Array:   "Int64ArrayCol",
		kindNumeric:     "DecimalCol",
		kindJSONB:       "JSONCol",
		kindTextArray:   "TextArrayCol",
		kindUUIDArray:   "UUIDArrayCol",
	}[c.kind]
	if !c.col.NotNull {
		return "Null" + base
	}
	return base
}
