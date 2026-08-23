package codegen

import (
	"sort"

	"github.com/gsoultan/raorm/schema"
)

// Relation codegen — what internal/planspike hand-wrote in P2, emitted.
//
// One plan type per *relation*, not per combination. That is finite by
// construction: a table with n relations gets n plan types, never 2^n, which is
// the explosion that killed the first design (R3). Named plans that load
// several relations at once, or nest, still need the plans.go front end; this
// is the tier below that, and it already carries both properties M3 exists for
// — two round trips, and an unloaded relation that does not compile.

// relPlan is one generated plan.
type relPlan struct {
	rel    *schema.Relation
	parent *schema.Table
	child  *schema.Table

	// Name is the plan's exported name in the context package: OrgWithUsers.
	Name string
	// ParentPkg and ChildPkg are the generated table packages.
	ParentPkg, ChildPkg string
	// ParentKey is the parent column the child's Column points at.
	ParentKey string
	// KeyGo is the Go type of the join key on both sides.
	KeyGo string

	// KeyNullable is whether the FOREIGN KEY COLUMN is nullable — which is not
	// the same as Relation.Nullable. Relation.Nullable describes the owning
	// side's Go field; for a has-many the column lives on the child, and a
	// self-referencing hierarchy is exactly the case where the two disagree:
	// Org.Children is a plain slice, but orgs.parent_id must be nullable or the
	// root has nowhere to point.
	KeyNullable bool
}

// relPlansFor enumerates the plans generatable for a context.
//
// Only relations whose *both* ends are in this generation are included: a plan
// spanning a table generated elsewhere cannot name its Row type, and guessing
// would produce a package that does not compile.
func relPlansFor(s *schema.Schema, tables []string) ([]relPlan, error) {
	in := make(map[string]bool, len(tables))
	for _, t := range tables {
		in[t] = true
	}

	var out []relPlan
	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		for _, rel := range t.Relations {
			if !in[rel.Target] {
				continue
			}
			child := s.Table(rel.Target)
			if child == nil {
				continue
			}
			p, err := planFor(t, child, rel)
			if err != nil {
				return nil, err
			}
			if p == nil {
				continue
			}
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func planFor(parent, child *schema.Table, rel *schema.Relation) (*relPlan, error) {
	parentPkg, err := PackageName(parent.GoName, parent.Name)
	if err != nil {
		return nil, err
	}
	childPkg, err := PackageName(child.GoName, child.Name)
	if err != nil {
		return nil, err
	}

	p := &relPlan{
		rel: rel, parent: parent, child: child,
		Name: exportName(parent.GoName) + "With" + exportName(rel.Field),
		// The row type carries the Row suffix so the constructor can take the
		// plain name: store.OrgWithUsers() yields []store.OrgWithUsersRow.
		// Pluralising the constructor instead gave OrgWithUserss().
		ParentPkg: parentPkg, ChildPkg: childPkg,
	}

	if rel.ToMany {
		// The key is on the child; the parent side is its primary key.
		if len(parent.PrimaryKey) != 1 {
			return nil, nil // composite keys need the plans.go front end
		}
		p.ParentKey = parent.PrimaryKey[0]
		col := child.Column(rel.Column)
		pk := parent.Column(p.ParentKey)
		if col == nil || pk == nil {
			return nil, nil
		}
		p.KeyGo = baseGoType(pk)
		p.KeyNullable = isNullable(col) // the child's column
		return p, nil
	}

	// To-one: the key is on the parent, pointing at the child's primary key.
	if len(child.PrimaryKey) != 1 {
		return nil, nil
	}
	p.ParentKey = child.PrimaryKey[0]
	pk := child.Column(p.ParentKey)
	own := parent.Column(rel.Column)
	if pk == nil || own == nil {
		return nil, nil
	}
	p.KeyGo = baseGoType(pk)
	p.KeyNullable = isNullable(own) // this table's own column
	return p, nil
}

// parentMethods redeclares the parent Query's builder on a plan.
//
// Go has no delegation, and embedding the Query would make Where() return the
// Query — dropping straight out of the plan. So every method has to be listed,
// and THE LIST IS THE BUG SURFACE: Order, Offset and After were added to Query
// after this list was written and were simply missing from every plan, so a
// plan could filter but not page. Anything added to Query belongs here too.
func (g *gen) parentMethods(q string, p relPlan) {
	pred := p.ParentPkg + ".Pred"
	for _, m := range []struct{ name, args, call string }{
		{"Where", "ps ..." + pred, "Where(ps...)"},
		{"WhereIf", "cond bool, pr " + pred, "WhereIf(cond, pr)"},
		{"Any", "ps ..." + pred, "Any(ps...)"},
		{"Not", "pr " + pred, "Not(pr)"},
		{"NotAny", "ps ..." + pred, "NotAny(ps...)"},
		{"Order", "ts ..." + p.ParentPkg + ".Sort", "Order(ts...)"},
		{"Limit", "n int64", "Limit(n)"},
		{"Offset", "n int64", "Offset(n)"},
	} {
		g.p("func (p %s) %s(%s) %s {", q, m.name, m.args, q)
		g.p("\tp.q = p.q.%s", m.call)
		g.p("\treturn p")
		g.p("}")
		g.p("")
	}
	g.p("// After pages the PARENTS past one already seen — keyset pagination over")
	g.p("// the plan. It takes the plan's row type, so the cursor is a row you")
	g.p("// actually received rather than one you had to unwrap.")
	g.p("func (p %s) After(r %sRow) %s {", q, p.Name, q)
	g.p("\tp.q = p.q.After(r.Row)")
	g.p("\treturn p")
	g.p("}")
	g.p("")
	g.p("// Err reports a parent query that outgrew its buffers or was given a")
	g.p("// mixed ordering to page. Terminals return it too; this is for checking")
	g.p("// a composed plan before running it.")
	g.p("func (p %s) Err() error { return p.q.Err() }", q)
	g.p("")
}

// emitRelPlans writes the plan layer into the context package.
func (g *gen) emitRelPlans(plans []relPlan) {
	if len(plans) == 0 {
		g.p("// No relation plans: this context declares no link whose other end is")
		g.p("// also generated here.")
		g.p("")
		return
	}

	for _, p := range plans {
		if p.rel.ToMany {
			g.emitToManyPlan(p)
			continue
		}
		g.emitToOnePlan(p)
	}
}

func (g *gen) emitToManyPlan(p relPlan) {
	parentField := exportName(p.parent.GoName)
	keyField := exportName(p.ParentKey)
	childKeyField := exportName(p.rel.Column)
	childHandle := exportName(p.rel.Column)

	g.p("// %sRow is %s with its %s loaded.", p.Name, p.parent.Name, p.rel.Field)
	g.p("//")
	g.p("// %s is a field HERE and nowhere else: %s.Row has no such field, so an", p.rel.Field, p.ParentPkg)
	g.p("// unloaded relation is not an empty slice, not a lazy fetch and not a")
	g.p("// lint warning — it does not compile.")
	g.p("type %sRow struct {", p.Name)
	g.p("\t%s.Row", p.ParentPkg)
	g.p("\t%s []%s.Row", exportName(p.rel.Field), p.ChildPkg)
	g.p("}")
	g.p("")

	q := p.Name + "Query"
	g.p("// %s builds the plan. Every builder method is redeclared rather than", q)
	g.p("// embedded: Go has no delegation, and an embedded Query would return")
	g.p("// itself from Where(), dropping straight out of the plan.")
	g.p("type %s struct {", q)
	g.p("\tq          %s.Query", p.ParentPkg)
	g.p("\tchildLimit int64")
	g.p("\tchildOrder []%s.Sort", p.ChildPkg)
	g.p("\tchildTop   int64")
	g.p("}")
	g.p("")
	g.p("// %s starts the plan.", p.Name)
	g.p("func %s() %s {", p.Name, q)
	g.p("\treturn %s{q: %s.New(), childLimit: defaultChildLimit}", q, p.ParentPkg)
	g.p("}")
	g.p("")
	g.parentMethods(q, p)
	g.p("// ChildLimit caps the total children fetched ACROSS ALL PARENTS.")
	g.p("//")
	g.p("// It is a guard, not a page size. Fifty parents with ChildLimit(100) is")
	g.p("// an error, not a hundred children each — the two queries fetch every")
	g.p("// child of every matched parent in one batch, and the limit only exists")
	g.p("// so that batch cannot silently come back partial.")
	g.p("//")
	g.p("// THERE IS NO PER-PARENT LIMIT. \"Each parent with its first twenty")
	g.p("// children\" is greatest-n-per-group, and doing it in two round trips")
	g.p("// needs LATERAL or row_number(); slicing in Go after the fact would")
	g.p("// fetch everything and only look like a limit. It is not built yet.")
	g.p("//")
	g.p("// To page ONE parent's children — the common case — query the child")
	g.p("// table directly, where Order, After and Limit all work:")
	g.p("//")
	g.p("//\t%s.New().Where(%s.%s.Eq(id)).Order(...).After(last).Limit(20)", p.ChildPkg, p.ChildPkg, exportName(p.rel.Column))
	g.p("func (p %s) ChildLimit(n int64) %s {", q, q)
	g.p("\tp.childLimit = n")
	g.p("\treturn p")
	g.p("}")
	g.p("")
	g.p("// ChildTop keeps at most n children PER PARENT — greatest-n-per-group,")
	g.p("// still two round trips.")
	g.p("//")
	g.p("// This is the per-parent limit ChildLimit is not. \"Fifty tenants, each")
	g.p("// with its five newest people\" is one query, not fifty, because the")
	g.p("// limit is expressed in SQL rather than by looping or by slicing")
	g.p("// afterwards — slicing would fetch every child and only look like a")
	g.p("// limit.")
	g.p("//")
	g.p("// It REQUIRES ChildOrder, and that ordering must be a strict total order.")
	g.p("// \"The first three by date\" with ties on that date returns an arbitrary")
	g.p("// three, and a different arbitrary three next call — a bug that only")
	g.p("// appears under data the developer did not have. Add the child's primary")
	g.p("// key as a final term if the natural ordering is not unique.")
	g.p("func (p %s) ChildTop(n int64) %s {", q, q)
	g.p("\tp.childTop = n")
	g.p("\treturn p")
	g.p("}")
	g.p("")
	g.p("// ChildOrder orders the children within each parent.")
	g.p("//")
	g.p("// Without it they arrive in the child table's default order, which is its")
	g.p("// primary key — defined, but almost never what a caller wanted to show.")
	g.p("func (p %s) ChildOrder(ts ...%s.Sort) %s {", q, p.ChildPkg, q)
	g.p("\tp.childOrder = ts")
	g.p("\treturn p")
	g.p("}")
	g.p("")

	g.p("// All runs the plan in exactly TWO round trips, whatever the parent count.")
	g.p("//")
	g.p("// The mechanism is `= ANY($1)`: one placeholder binds the whole id list, so")
	g.p("// fifty parents and five thousand produce the same SQL and share one")
	g.p("// compiled statement. No join is involved, which is why M3 was never")
	g.p("// actually blocked on join support.")
	g.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", q, p.Name)
	g.p("\tparents, err := p.q.All(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif len(parents) == 0 {")
	g.p("\t\t// Round two would be `= ANY('{}')`, a guaranteed-empty query. Not")
	g.p("\t\t// issuing it is the difference between costing 2 round trips when")
	g.p("\t\t// there is work and 2 when there is none.")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\tout := make([]%sRow, len(parents))", p.Name)
	g.p("\tids := make([]%s, len(parents))", p.KeyGo)
	g.p("\tat := make(map[%s]int, len(parents))", p.KeyGo)
	g.p("\tfor i, r := range parents {")
	g.p("\t\tout[i] = %sRow{Row: r}", p.Name)
	g.p("\t\tids[i] = r.%s", keyField)
	g.p("\t\tat[r.%s] = i", keyField)
	g.p("\t}")
	g.p("\tvar kids []%s.Row", p.ChildPkg)
	g.p("\tif p.childTop > 0 {")
	g.p("\t\t// Per-parent limit: one query with the limit expressed in SQL.")
	g.p("\t\tkids, err = %s.BatchTopBy%s(ctx, ex, ids, p.childTop, p.childOrder...)", p.ChildPkg, exportName(p.rel.Column))
	g.p("\t} else {")
	g.p("\t\tcq := %s.New().Where(%s.%s.In(ids...)).Limit(p.childLimit)", p.ChildPkg, p.ChildPkg, childHandle)
	g.p("\t\tif len(p.childOrder) > 0 {")
	g.p("\t\t\tcq = cq.Order(p.childOrder...)")
	g.p("\t\t}")
	g.p("\t\tkids, err = cq.All(ctx, ex, nil)")
	g.p("\t}")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\t// A partial relation load is worse than a failed one: every count")
	g.p("\t// computed from it is wrong and nothing says so. A per-parent load")
	g.p("\t// is bounded by construction, so the global guard does not apply.")
	g.p("\tif p.childTop == 0 && int64(len(kids)) >= p.childLimit {")
	g.p("\t\treturn nil, runtime.ErrChildLimit")
	g.p("\t}")
	g.p("\tfor _, k := range kids {")
	if p.KeyNullable {
		g.p("\t\tkey, ok := k.%s.Get()", childKeyField)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
		g.p("\t\tif i, ok := at[key]; ok {")
	} else {
		g.p("\t\tif i, ok := at[k.%s]; ok {", childKeyField)
	}
	g.p("\t\t\tout[i].%s = append(out[i].%s, k)", exportName(p.rel.Field), exportName(p.rel.Field))
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn out, nil")
	g.p("}")
	g.p("")
	_ = parentField
}

func (g *gen) emitToOnePlan(p relPlan) {
	ownField := exportName(p.rel.Column)
	childKeyField := exportName(p.ParentKey)
	childHandle := exportName(p.ParentKey)
	q := p.Name + "Query"

	g.p("// %sRow is %s with its %s loaded.", p.Name, p.parent.Name, p.rel.Field)
	g.p("type %sRow struct {", p.Name)
	g.p("\t%s.Row", p.ParentPkg)
	if p.KeyNullable {
		g.p("\t// A pointer because the link is optional. nil means the row has no")
		g.p("\t// %s, which is different from having one that failed to load.", p.rel.Field)
		g.p("\t%s *%s.Row", exportName(p.rel.Field), p.ChildPkg)
	} else {
		g.p("\t%s %s.Row", exportName(p.rel.Field), p.ChildPkg)
	}
	g.p("}")
	g.p("")
	g.p("type %s struct {", q)
	g.p("\tq %s.Query", p.ParentPkg)
	g.p("}")
	g.p("")
	g.p("// %s starts the plan.", p.Name)
	g.p("func %s() %s { return %s{q: %s.New()} }", p.Name, q, q, p.ParentPkg)
	g.p("")
	g.parentMethods(q, p)
	g.p("// All runs the plan in exactly TWO round trips. Distinct parent keys are")
	g.p("// de-duplicated before the second, so a thousand rows pointing at three")
	g.p("// orgs fetch three orgs.")
	g.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", q, p.Name)
	g.p("\tparents, err := p.q.All(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif len(parents) == 0 {")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\tout := make([]%sRow, len(parents))", p.Name)
	g.p("\tseen := make(map[%s]bool, len(parents))", p.KeyGo)
	g.p("\tids := make([]%s, 0, len(parents))", p.KeyGo)
	g.p("\tfor i, r := range parents {")
	g.p("\t\tout[i] = %sRow{Row: r}", p.Name)
	if p.KeyNullable {
		g.p("\t\tkey, ok := r.%s.Get()", ownField)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
	} else {
		g.p("\t\tkey := r.%s", ownField)
	}
	g.p("\t\tif !seen[key] {")
	g.p("\t\t\tseen[key] = true")
	g.p("\t\t\tids = append(ids, key)")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tif len(ids) == 0 {")
	g.p("\t\treturn out, nil")
	g.p("\t}")
	g.p("\ttargets, err := %s.New().", p.ChildPkg)
	g.p("\t\tWhere(%s.%s.In(ids...)).", p.ChildPkg, childHandle)
	g.p("\t\tLimit(int64(len(ids))).")
	g.p("\t\tAll(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tby := make(map[%s]int, len(targets))", p.KeyGo)
	g.p("\tfor i := range targets {")
	g.p("\t\tby[targets[i].%s] = i", childKeyField)
	g.p("\t}")
	g.p("\tfor i := range out {")
	if p.KeyNullable {
		g.p("\t\tkey, ok := out[i].%s.Get()", ownField)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
	} else {
		g.p("\t\tkey := out[i].%s", ownField)
	}
	g.p("\t\tj, ok := by[key]")
	g.p("\t\tif !ok {")
	g.p("\t\t\t// A foreign key pointing at a row that is not there. The database")
	g.p("\t\t\t// forbids it, so reaching this means the constraint was dropped.")
	g.p("\t\t\treturn nil, fmt.Errorf(\"raorm: %%s references a missing %%s row\", %q, %q)", p.parent.Name, p.child.Name)
	g.p("\t\t}")
	if p.KeyNullable {
		g.p("\t\tout[i].%s = &targets[j]", exportName(p.rel.Field))
	} else {
		g.p("\t\tout[i].%s = targets[j]", exportName(p.rel.Field))
	}
	g.p("\t}")
	g.p("\treturn out, nil")
	g.p("}")
	g.p("")
}
