package codegen

import (
	"github.com/gsoultan/raorm/schema"
)

// Exclusive-arc emission.
//
// The columns are ordinary nullable foreign keys and the read path already
// handles them. What has to be generated is everything that makes the arc a
// TYPE rather than three columns that happen to be related: a predicate per
// variant, and a match that stops compiling when a variant is added.

func (g *gen) arcs() {
	for _, arc := range g.t.Arcs {
		g.arcPredicates(arc)
		g.arcMatch(arc)
	}
}

// arcPredicates emits one predicate per variant.
//
// They lower to `post_id IS NOT NULL` here, and would lower to
// `subject_type = 'post'` under a discriminator. The call site does not change
// when the strategy does — which is the point of naming the variant rather than
// the column.
func (g *gen) arcPredicates(arc *schema.Arc) {
	field := exportName(arc.Field)
	for _, v := range arc.Variants {
		i := g.colIndex(v.Column)
		if i < 0 {
			continue
		}
		g.p("// %sIs%s matches rows whose %s is a %s.", field, v.GoName, arc.Field, v.Table)
		g.p("func %sIs%s() Pred {", field, v.GoName)
		g.p("\treturn Pred{col: %d, op: opIsNotNull}", i)
		g.p("}")
		g.p("")
	}
}

// arcMatch emits the exhaustive match.
//
// A Go type switch is not exhaustive: add a variant and every switch quietly
// falls through to default, which is a silent behaviour change in every caller.
// Match takes one function per variant, so ADDING A VARIANT CHANGES THE ARITY
// and every call site fails to compile. That is the same instinct as ADR-0003 —
// turn a runtime surprise into a build error.
func (g *gen) arcMatch(arc *schema.Arc) {
	field := exportName(arc.Field)

	g.p("// Match%s dispatches on which variant this row's %s is.", field, arc.Field)
	g.p("//")
	g.p("// One function per variant, so adding a variant to the model breaks every")
	g.p("// call site at compile time. A type switch would fall through to default")
	g.p("// and change behaviour silently.")
	g.p("//")
	g.p("// Each function receives the variant's id. The row itself is not loaded —")
	g.p("// that is a fetch plan's job, and doing it here would hide a query inside")
	g.p("// what looks like a switch.")
	args := make([]string, 0, len(arc.Variants))
	for _, v := range arc.Variants {
		args = append(args, lowerFirst(v.GoName)+" func("+arcKeyType(g.t, v)+") R")
	}
	if arc.Optional {
		args = append(args, "none func() R")
	}
	g.p("func Match%s[R any](r Row, %s) R {", field, joinStr(args, ", "))
	for _, v := range arc.Variants {
		f := exportName(v.Column)
		g.p("\tif id, ok := r.%s.Get(); ok {", f)
		g.p("\t\treturn %s(id)", lowerFirst(v.GoName))
		g.p("\t}")
	}
	if arc.Optional {
		g.p("\treturn none()")
	} else {
		g.p("\t// Unreachable while the CHECK holds: exactly one variant is set. It")
		g.p("\t// is reachable if somebody dropped the constraint, and a zero value")
		g.p("\t// would be a wrong answer rather than a loud one.")
		g.p("\tpanic(%q)", "raorm: "+g.t.Name+"."+arc.Field+
			" has no variant set — the exactly-one CHECK is missing or was dropped")
	}
	g.p("}")
	g.p("")

	// A name for the variant is wanted often enough that every caller writing
	// the same Match is worse than generating it once.
	g.p("// %sVariant names which variant is set, for logs and errors.", field)
	g.p("func %sVariant(r Row) string {", field)
	call := make([]string, 0, len(arc.Variants))
	for _, v := range arc.Variants {
		call = append(call, "func("+arcKeyType(g.t, v)+") string { return "+quoteGo(v.Table)+" }")
	}
	if arc.Optional {
		call = append(call, "func() string { return \"\" }")
	}
	g.p("\treturn Match%s(r, %s)", field, joinStr(call, ", "))
	g.p("}")
	g.p("")
}

// arcKeyType is the Go type of a variant's id, read off the table that OWNS the
// arc.
//
// The table is a parameter rather than g.t because the context-file generator
// has no table of its own — it emits across every table in the context — and
// reading g.t there is how the first cut of arcLoader dereferenced nothing.
func arcKeyType(owner *schema.Table, v schema.ArcVariant) string {
	if c := owner.Column(v.Column); c != nil {
		return baseGoType(c)
	}
	return "[16]byte"
}

func quoteGo(s string) string { return `"` + s + `"` }

// arcLoaderFor emits, in the CONTEXT package, a loader that fetches every
// variant of an arc in ONE round trip.
//
// One query per variant table is unavoidable — they are different tables with
// different columns — but they need not be different round trips. The Executor
// port has Batch precisely so N statements cost one conversation, and an arc is
// the case that makes it pay: without it, "load these attachments' subjects" is
// one round trip per variant, which is an N+1 in the number of variants rather
// than the number of rows, and just as avoidable.
func (g *gen) arcLoader(arcOwnerPkg string, t *schema.Table, arc *schema.Arc, variantPkg map[string]string) {
	field := exportName(arc.Field)
	name := exportName(t.GoName) + "With" + field

	g.p("// %sRow is %s with its %s resolved.", name, t.Name, arc.Field)
	g.p("//")
	g.p("// One field per variant, at most one of them non-nil — the same shape the")
	g.p("// database enforces. A single interface field would be smaller to write")
	g.p("// and would give every caller a type assertion to get wrong.")
	g.p("type %sRow struct {", name)
	g.p("\t%s.Row", arcOwnerPkg)
	for _, v := range arc.Variants {
		g.p("\t%s *%s.Row", v.GoName, variantPkg[v.Table])
	}
	g.p("}")
	g.p("")

	q := name + "Query"
	g.p("type %s struct {", q)
	g.p("\tq %s.Query", arcOwnerPkg)
	g.p("}")
	g.p("")
	g.p("// %s starts the plan. TWO round trips: one for the rows, one BATCHED", name)
	g.p("// query carrying every variant lookup.")
	g.p("func %s() %s { return %s{q: %s.New()} }", name, q, q, arcOwnerPkg)
	g.p("")
	g.parentMethods(q, relPlan{Name: name, ParentPkg: arcOwnerPkg})

	g.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", q, name)
	g.p("\trows, err := p.q.All(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif len(rows) == 0 {")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\tout := make([]%sRow, len(rows))", name)
	for i, v := range arc.Variants {
		g.p("\tids%d := make([]%s, 0, len(rows))", i, arcKeyType(t, v))
	}
	g.p("\tfor i, r := range rows {")
	g.p("\t\tout[i] = %sRow{Row: r}", name)
	for i, v := range arc.Variants {
		g.p("\t\tif id, ok := r.%s.Get(); ok {", exportName(v.Column))
		g.p("\t\t\tids%d = append(ids%d, id)", i, i)
		g.p("\t\t}")
	}
	g.p("\t}")
	g.p("")
	g.p("\t// One BatchOp per variant that has any ids at all: a variant nothing")
	g.p("\t// referenced costs no statement, so an arc used one way at a time is")
	g.p("\t// as cheap as a plain relation.")
	g.p("\tvar ops []runtime.BatchOp")
	g.p("\tvar which []int")
	for i, v := range arc.Variants {
		pkg := variantPkg[v.Table]
		g.p("\tif len(ids%d) > 0 {", i)
		g.p("\t\tbnd%d := %s.GetBinder()", i, pkg)
		g.p("\t\tdefer %s.PutBinder(bnd%d)", pkg, i)
		g.p("\t\t// The binder owns the argument slice, so it must outlive the")
		g.p("\t\t// batch — releasing it before Batch runs would hand the driver")
		g.p("\t\t// arguments another goroutine may already be overwriting.")
		g.p("\t\tsql%d, args%d := %s.New().Where(%s.ID.In(ids%d...)).Limit(int64(len(ids%d))).Prepare(bnd%d)",
			i, i, pkg, pkg, i, i, i)
		g.p("\t\tops = append(ops, runtime.BatchOp{SQL: sql%d, Args: args%d, WantRows: true})", i, i)
		g.p("\t\twhich = append(which, %d)", i)
		g.p("\t}")
	}
	g.p("\tif len(ops) == 0 {")
	g.p("\t\treturn out, nil")
	g.p("\t}")
	g.p("")
	for i, v := range arc.Variants {
		pkg := variantPkg[v.Table]
		g.p("\tby%d := make(map[%s]int, len(ids%d))", i, arcKeyType(t, v), i)
		g.p("\tvar got%d []%s.Row", i, pkg)
	}
	g.p("\terr = ex.Batch(ctx, ops, func(n int, rs runtime.Rows, _ int64, err error) error {")
	g.p("\t\tif err != nil {")
	g.p("\t\t\treturn err")
	g.p("\t\t}")
	g.p("\t\tswitch which[n] {")
	for i, v := range arc.Variants {
		pkg := variantPkg[v.Table]
		g.p("\t\tcase %d:", i)
		g.p("\t\t\tvar sl runtime.Slab")
		g.p("\t\t\tfor rs.Next() {")
		g.p("\t\t\t\tgot%d = append(got%d, %s.Row{})", i, i, pkg)
		g.p("\t\t\t\tif err := %s.Scan(rs.RawValues(), &got%d[len(got%d)-1], &sl); err != nil {", pkg, i, i)
		g.p("\t\t\t\t\treturn err")
		g.p("\t\t\t\t}")
		g.p("\t\t\t}")
	}
	g.p("\t\t}")
	g.p("\t\treturn nil")
	g.p("\t})")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("")
	for i := range arc.Variants {
		g.p("\tfor j := range got%d {", i)
		g.p("\t\tby%d[got%d[j].ID] = j", i, i)
		g.p("\t}")
	}
	g.p("\tfor i := range out {")
	for i, v := range arc.Variants {
		g.p("\t\tif id, ok := out[i].%s.Get(); ok {", exportName(v.Column))
		g.p("\t\t\tif j, ok := by%d[id]; ok {", i)
		g.p("\t\t\t\tout[i].%s = &got%d[j]", v.GoName, i)
		g.p("\t\t\t}")
		g.p("\t\t}")
	}
	g.p("\t}")
	g.p("\treturn out, nil")
	g.p("}")
	g.p("")
}
