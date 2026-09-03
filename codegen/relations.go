package codegen

import (
	"fmt"
	"sort"

	"github.com/gsoultan/storm/schema"
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

	// The many-to-many fields, all empty for every other shape.
	//
	// LinkPkg is the join table's generated package. LinkParentCol and
	// LinkChildCol are its two key columns, and ChildKey is the far table's
	// primary key that LinkChildCol points at. A link member costs TWO round
	// trips, not one: the join rows, then the far side.
	LinkPkg       string
	LinkParentCol string
	LinkChildCol  string
	ChildKey      string
	ChildKeyGo    string
}

// isLink reports whether this plan loads through a join table.
func (p relPlan) isLink() bool { return p.LinkPkg != "" }

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
			p, err := planFor(s, t, child, rel)
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

func planFor(s *schema.Schema, parent, child *schema.Table, rel *schema.Relation) (*relPlan, error) {
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

	if rel.ToMany && rel.Link != "" {
		// Many-to-many: neither side carries a key, so the mapping is read
		// from the join table first and the far side fetched by primary key.
		// Two round trips for this member rather than one, which `storm lint`
		// counts and a reviewer should be able to read off the plan.
		if len(parent.PrimaryKey) != 1 || len(child.PrimaryKey) != 1 {
			return nil, nil // a composite key on either end needs an explicit join
		}
		// Resolved from the link TABLE, through the same call that names the
		// package when it is generated. Recomputing the name here instead is
		// how the plan came to reference mgpostmgtag while the package was
		// mgpostmgtags.
		lt := s.Table(rel.Link)
		if lt == nil {
			return nil, fmt.Errorf(
				"codegen: %s.%s is a many-to-many through %s, which is not in this context",
				parent.Name, rel.Field, rel.Link)
		}
		linkPkg, err := PackageName(lt.GoName, lt.Name)
		if err != nil {
			return nil, err
		}
		p.ParentKey = parent.PrimaryKey[0]
		p.ChildKey = child.PrimaryKey[0]
		pk := parent.Column(p.ParentKey)
		if pk == nil || child.Column(p.ChildKey) == nil {
			return nil, nil
		}
		p.KeyGo = baseGoType(pk)
		p.ChildKeyGo = baseGoType(child.Column(p.ChildKey))
		p.LinkPkg = linkPkg
		p.LinkParentCol = rel.LinkColumn
		p.LinkChildCol = rel.LinkTargetColumn
		return p, nil
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

// namedPlansFor turns each declared plan into the relation plans it loads.
//
// A plan naming a relation whose other end is generated elsewhere is a
// generation error, not a silently smaller plan: the developer asked for that
// relation by name, and quietly dropping it is how an N+1 gets reintroduced by
// a package boundary.
func namedPlansFor(s *schema.Schema, tables []string) ([]namedPlan, error) {
	in := make(map[string]bool, len(tables))
	for _, t := range tables {
		in[t] = true
	}
	var out []namedPlan
	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		for _, pl := range t.Plans {
			np := namedPlan{parent: t, Name: exportName(t.GoName) + pl.Name}
			pkg, err := PackageName(t.GoName, t.Name)
			if err != nil {
				return nil, err
			}
			np.ParentPkg = pkg
			for _, field := range pl.Fields {
				m, err := planMember(s, in, t, pl.Name, field, np.Name)
				if err != nil {
					return nil, err
				}
				np.members = append(np.members, *m)
			}
			out = append(out, np)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// namedPlan is a declared plan: one parent, several relations loaded together.
type namedPlan struct {
	parent    *schema.Table
	Name      string
	ParentPkg string
	members   []planMemberT
}

// planMemberT is one relation of a plan, plus anything loaded through it.
//
// A nested member needs a row type of its own — a post inside a user's feed is
// a post WITH its comments, and that is a different type from a bare post. The
// name is the plan's plus the path, so UserFeed's Posts become UserFeedPosts.
type planMemberT struct {
	relPlan
	RowType string // "" means the child's own Row is enough
	Nested  []planMemberT
}

// planMember resolves one plan field and everything under it.
func planMember(s *schema.Schema, in map[string]bool, parent *schema.Table,
	planName string, f schema.PlanField, typePrefix string) (*planMemberT, error) {

	rel := relationByField(parent, f.Field)
	if rel == nil {
		return nil, fmt.Errorf(
			"codegen: table %s plan %s names %s, which is not a relation",
			parent.Name, planName, f.Field)
	}
	if !in[rel.Target] {
		return nil, fmt.Errorf(
			"codegen: table %s plan %s loads %s from table %s, which is not generated "+
				"in this context — split the plan or generate both tables together",
			parent.Name, planName, f.Field, rel.Target)
	}
	child := s.Table(rel.Target)
	base, err := planFor(s, parent, child, rel)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf(
			"codegen: table %s plan %s cannot load %s — a composite key needs an explicit join",
			parent.Name, planName, f.Field)
	}

	m := &planMemberT{relPlan: *base}
	if len(f.Nested) == 0 {
		return m, nil
	}
	// Nesting through a to-one is not generated. It is expressible — load the
	// target, then its relations — but it makes the row type a pointer to a
	// type that itself has relations, and every caller then has two nil checks
	// to get one value. Declare the far relation on the plan directly instead.
	if !rel.ToMany {
		return nil, fmt.Errorf(
			"codegen: table %s plan %s nests through %s, which is a to-one relation — "+
				"load its relations from the plan directly rather than through it",
			parent.Name, planName, f.Field)
	}
	m.RowType = typePrefix + exportName(f.Field)
	for _, sub := range f.Nested {
		nm, err := planMember(s, in, child, planName, sub, m.RowType)
		if err != nil {
			return nil, err
		}
		m.Nested = append(m.Nested, *nm)
	}
	return m, nil
}

func relationByField(t *schema.Table, field string) *schema.Relation {
	for _, r := range t.Relations {
		if r.Field == field {
			return r
		}
	}
	return nil
}

// emitNamedPlans writes one type per declared plan.
func (g *gen) emitNamedPlans(plans []namedPlan) {
	for _, np := range plans {
		g.emitNamedPlan(np)
	}
}

func (g *gen) emitNamedPlan(np namedPlan) {
	q := np.Name + "Query"

	// Nested row types first: a plan's row type embeds them.
	g.emitNestedTypes(np.members)
	g.p("// %sRow is %s with %d relation(s) loaded.", np.Name, np.parent.Name, len(np.members))
	g.p("//")
	g.p("// One type per DECLARED plan. Generating a type per With(...)")
	g.p("// combination would be 2^n per entity; you get the plans you named.")
	g.p("type %sRow struct {", np.Name)
	g.p("	%s.Row", np.ParentPkg)
	for _, m := range np.members {
		g.p("	%s", planField(m))
	}
	g.p("}")
	g.p("")
	g.p("type %s struct {", q)
	g.p("	q %s.Query", np.ParentPkg)
	g.p("	childLimit int64")
	g.p("}")
	g.p("")
	g.p("// %s starts the plan. It costs %d round trips: one for the parents and", np.Name, len(np.members)+1)
	g.p("// one per relation, whatever the row count.")
	g.p("func %s() %s {", np.Name, q)
	g.p("	return %s{q: %s.New(), childLimit: defaultChildLimit}", q, np.ParentPkg)
	g.p("}")
	g.p("")
	g.parentMethods(q, relPlan{Name: np.Name, ParentPkg: np.ParentPkg})
	g.p("// ChildLimit caps each relation's fetch. A guard, not a page size.")
	g.p("func (p %s) ChildLimit(n int64) %s {", q, q)
	g.p("	p.childLimit = n")
	g.p("	return p")
	g.p("}")
	g.p("")

	g.p("// All runs the plan in %d round trips.", len(np.members)+1)
	g.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", q, np.Name)
	g.p("	parents, err := p.q.All(ctx, ex, nil)")
	g.p("	if err != nil {")
	g.p("		return nil, err")
	g.p("	}")
	g.p("	if len(parents) == 0 {")
	g.p("		return nil, nil")
	g.p("	}")
	g.p("	out := make([]%sRow, len(parents))", np.Name)
	g.p("	for i, r := range parents {")
	g.p("		out[i] = %sRow{Row: r}", np.Name)
	g.p("	}")
	for i, m := range np.members {
		g.p("	if err := p.load%d(ctx, ex, out); err != nil {", i)
		g.p("		return nil, err")
		g.p("	}")
		_ = m
	}
	g.p("	return out, nil")
	g.p("}")
	g.p("")

	for i, m := range np.members {
		g.emitNamedMember(np, m, i, q)
	}
}

// planField is the struct field one relation contributes.
func planField(m planMemberT) string {
	name := exportName(m.rel.Field)
	elem := m.ChildPkg + ".Row"
	if m.RowType != "" {
		elem = m.RowType // a nested member carries its own relations
	}
	if m.rel.ToMany {
		return name + " []" + elem
	}
	if m.KeyNullable {
		return name + " *" + elem
	}
	return name + " " + elem
}

// emitNestedTypes writes a row type per nested member, depth first, so a type
// is declared before the one embedding it.
func (g *gen) emitNestedTypes(members []planMemberT) {
	for _, m := range members {
		if m.RowType == "" {
			continue
		}
		g.emitNestedTypes(m.Nested)
		g.p("// %s is a %s with its own relations loaded. A %s inside this plan is",
			m.RowType, m.child.Name, m.child.Name)
		g.p("// not the same type as a bare one — the extra fields exist only where")
		g.p("// the plan said to load them, which is the guarantee one level down.")
		g.p("type %s struct {", m.RowType)
		g.p("\t%s.Row", m.ChildPkg)
		for _, sub := range m.Nested {
			g.p("\t%s", planField(sub))
		}
		g.p("}")
		g.p("")
	}
}

// emitNamedMember writes the loader for one relation of a plan.
func (g *gen) emitNamedMember(np namedPlan, m planMemberT, i int, q string) {
	g.emitMemberLoader(q, fmt.Sprintf("load%d", i), np.Name+"Row", np.parent.Name, m)
}

// emitLinkLoader writes a MANY-TO-MANY fetch: the join rows, then the far side
// by primary key, then the attach.
//
// Two queries rather than one join, and deliberately. A join would return the
// far row once per parent that references it — the same tag repeated across
// every post carrying it — which is the row multiplication a batch loader
// exists to avoid. Fetching distinct far keys sends each row once, and the
// second query is bounded by the number of DISTINCT children, not by the
// number of links.
//
// Two round trips for this member, not one. That is what a join table costs,
// it is fixed rather than per-parent, and `storm lint` counts it.
func (g *gen) emitLinkLoader(q, fn, rowType, field string, m planMemberT) {
	parentKey := exportName(m.ParentKey)
	linkParent := exportName(m.LinkParentCol)
	linkChild := exportName(m.LinkChildCol)
	childKey := exportName(m.ChildKey)

	g.p("\tids := make([]%s, len(out))", m.KeyGo)
	g.p("\tat := make(map[%s]int, len(out))", m.KeyGo)
	g.p("\tfor i := range out {")
	g.p("\t\tids[i] = out[i].%s", parentKey)
	g.p("\t\tat[out[i].%s] = i", parentKey)
	g.p("\t}")

	g.p("\tlinks, err := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(p.childLimit).All(ctx, ex, nil)",
		m.LinkPkg, m.LinkPkg, linkParent)
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tif int64(len(links)) >= p.childLimit {")
	g.p("\t\treturn runtime.ErrChildLimit")
	g.p("\t}")
	// No links is not an empty second query: it is no second query at all,
	// the same rule an empty parent set already follows.
	g.p("\tif len(links) == 0 {")
	g.p("\t\treturn nil")
	g.p("\t}")

	g.p("\tseen := make(map[%s]bool, len(links))", m.ChildKeyGo)
	g.p("\tfar := make([]%s, 0, len(links))", m.ChildKeyGo)
	g.p("\tfor _, l := range links {")
	g.p("\t\tif !seen[l.%s] {", linkChild)
	g.p("\t\t\tseen[l.%s] = true", linkChild)
	g.p("\t\t\tfar = append(far, l.%s)", linkChild)
	g.p("\t\t}")
	g.p("\t}")

	g.p("\tkids, err := %s.New().Unordered().Where(%s.%s.In(far...)).Limit(int64(len(far))).All(ctx, ex, nil)",
		m.ChildPkg, m.ChildPkg, childKey)
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tbyKey := make(map[%s]%s.Row, len(kids))", m.ChildKeyGo, m.ChildPkg)
	g.p("\tfor _, k := range kids {")
	g.p("\t\tbyKey[k.%s] = k", childKey)
	g.p("\t}")

	// Attach in LINK order, so a parent's children come back in the order the
	// join table returned them rather than in whatever order the second query
	// happened to produce.
	g.p("\tfor _, l := range links {")
	g.p("\t\tj, ok := at[l.%s]", linkParent)
	g.p("\t\tif !ok {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tk, ok := byKey[l.%s]", linkChild)
	g.p("\t\tif !ok {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	if m.RowType != "" {
		g.p("\t\tout[j].%s = append(out[j].%s, %s{Row: k})", field, field, m.RowType)
	} else {
		g.p("\t\tout[j].%s = append(out[j].%s, k)", field, field)
	}
	g.p("\t}")

	for i := range m.Nested {
		g.p("\tif err := p.%s_%d(ctx, ex, out); err != nil {", fn, i)
		g.p("\t\treturn err")
		g.p("\t}")
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	for i, sub := range m.Nested {
		g.emitNestedPass(q, fmt.Sprintf("%s_%d", fn, i), rowType, field, m.RowType, sub)
	}
}

// emitMemberLoader writes one relation's fetch-and-attach over a slice of
// rowType, then recurses for anything nested through it.
func (g *gen) emitMemberLoader(q, fn, rowType, parentTable string, m planMemberT) {
	field := exportName(m.rel.Field)

	g.p("// %s fetches %s.", fn, m.rel.Field)
	g.p("func (p %s) %s(ctx context.Context, ex runtime.Executor, out []%s) error {", q, fn, rowType)

	if m.isLink() {
		g.emitLinkLoader(q, fn, rowType, field, m)
		return
	}

	if m.rel.ToMany {
		keyField := exportName(m.ParentKey)
		childKey := exportName(m.rel.Column)
		g.p("\tids := make([]%s, len(out))", m.KeyGo)
		g.p("\tat := make(map[%s]int, len(out))", m.KeyGo)
		g.p("\tfor i := range out {")
		g.p("\t\tids[i] = out[i].%s", keyField)
		g.p("\t\tat[out[i].%s] = i", keyField)
		g.p("\t}")
		g.p("\tkids, err := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(p.childLimit).All(ctx, ex, nil)",
			m.ChildPkg, m.ChildPkg, childKey)
		g.p("\tif err != nil {")
		g.p("\t\treturn err")
		g.p("\t}")
		g.p("\tif int64(len(kids)) >= p.childLimit {")
		g.p("\t\treturn runtime.ErrChildLimit")
		g.p("\t}")
		g.p("\tfor _, k := range kids {")
		if m.KeyNullable {
			g.p("\t\tkey, ok := k.%s.Get()", childKey)
			g.p("\t\tif !ok {")
			g.p("\t\t\tcontinue")
			g.p("\t\t}")
			g.p("\t\tif j, ok := at[key]; ok {")
		} else {
			g.p("\t\tif j, ok := at[k.%s]; ok {", childKey)
		}
		if m.RowType != "" {
			g.p("\t\t\tout[j].%s = append(out[j].%s, %s{Row: k})", field, field, m.RowType)
		} else {
			g.p("\t\t\tout[j].%s = append(out[j].%s, k)", field, field)
		}
		g.p("\t\t}")
		g.p("\t}")
		for i := range m.Nested {
			g.p("\tif err := p.%s_%d(ctx, ex, out); err != nil {", fn, i)
			g.p("\t\treturn err")
			g.p("\t}")
		}
		g.p("\treturn nil")
		g.p("}")
		g.p("")
		// Anything nested through this relation loads from the children just
		// attached, so it walks the parents once more to gather them — cheap,
		// and it keeps the round-trip count at one per relation.
		for i, sub := range m.Nested {
			g.emitNestedPass(q, fmt.Sprintf("%s_%d", fn, i), rowType, field, m.RowType, sub)
		}
		return
	}

	own := exportName(m.rel.Column)
	childKey := exportName(m.ParentKey)
	g.p("\tseen := make(map[%s]bool, len(out))", m.KeyGo)
	g.p("\tids := make([]%s, 0, len(out))", m.KeyGo)
	g.p("\tfor i := range out {")
	if m.KeyNullable {
		g.p("\t\tkey, ok := out[i].%s.Get()", own)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
	} else {
		g.p("\t\tkey := out[i].%s", own)
	}
	g.p("\t\tif !seen[key] {")
	g.p("\t\t\tseen[key] = true")
	g.p("\t\t\tids = append(ids, key)")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tif len(ids) == 0 {")
	g.p("\t\treturn nil")
	g.p("\t}")
	g.p("\ttargets, err := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(int64(len(ids))).All(ctx, ex, nil)",
		m.ChildPkg, m.ChildPkg, childKey)
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tby := make(map[%s]int, len(targets))", m.KeyGo)
	g.p("\tfor i := range targets {")
	g.p("\t\tby[targets[i].%s] = i", childKey)
	g.p("\t}")
	g.p("\tfor i := range out {")
	if m.KeyNullable {
		g.p("\t\tkey, ok := out[i].%s.Get()", own)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
	} else {
		g.p("\t\tkey := out[i].%s", own)
	}
	g.p("\t\tj, ok := by[key]")
	g.p("\t\tif !ok {")
	g.p("\t\t\treturn fmt.Errorf(\"storm: %%s references a missing %%s row\", %q, %q)",
		parentTable, m.child.Name)
	g.p("\t\t}")
	if m.KeyNullable {
		g.p("\t\tout[i].%s = &targets[j]", field)
	} else {
		g.p("\t\tout[i].%s = targets[j]", field)
	}
	g.p("\t}")
	g.p("\treturn nil")
	g.p("}")
	g.p("")
}

// emitNestedPass loads a relation of a relation: it walks every parent's
// already-attached children, gathers their keys, and fetches in one query.
//
// One query per relation, whatever the nesting depth — the walk is in memory
// and the round trips are what the plan promised.
func (g *gen) emitNestedPass(q, fn, outerRow, outerField, innerRow string, m planMemberT) {
	field := exportName(m.rel.Field)

	g.p("// %s fetches %s, through each %s.", fn, m.rel.Field, outerField)
	g.p("func (p %s) %s(ctx context.Context, ex runtime.Executor, out []%s) error {", q, fn, outerRow)
	g.p("\t// Every child of every parent, flattened once so the fetch is one query.")
	g.p("\tn := 0")
	g.p("\tfor i := range out {")
	g.p("\t\tn += len(out[i].%s)", outerField)
	g.p("\t}")
	g.p("\tif n == 0 {")
	g.p("\t\treturn nil")
	g.p("\t}")

	if !m.rel.ToMany {
		g.err = fmt.Errorf("codegen: nesting a to-one relation is not generated")
		return
	}

	keyField := exportName(m.ParentKey)
	childKey := exportName(m.rel.Column)
	g.p("\tids := make([]%s, 0, n)", m.KeyGo)
	g.p("\ttype at%s struct{ i, j int }", fn)
	g.p("\tat := make(map[%s]at%s, n)", m.KeyGo, fn)
	g.p("\tfor i := range out {")
	g.p("\t\tfor j := range out[i].%s {", outerField)
	g.p("\t\t\tk := out[i].%s[j].%s", outerField, keyField)
	g.p("\t\t\tids = append(ids, k)")
	g.p("\t\t\tat[k] = at%s{i, j}", fn)
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tkids, err := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(p.childLimit).All(ctx, ex, nil)",
		m.ChildPkg, m.ChildPkg, childKey)
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tif int64(len(kids)) >= p.childLimit {")
	g.p("\t\treturn runtime.ErrChildLimit")
	g.p("\t}")
	g.p("\tfor _, k := range kids {")
	if m.KeyNullable {
		g.p("\t\tkey, ok := k.%s.Get()", childKey)
		g.p("\t\tif !ok {")
		g.p("\t\t\tcontinue")
		g.p("\t\t}")
		g.p("\t\tif a, ok := at[key]; ok {")
	} else {
		g.p("\t\tif a, ok := at[k.%s]; ok {", childKey)
	}
	if m.RowType != "" {
		g.p("\t\t\tout[a.i].%s[a.j].%s = append(out[a.i].%s[a.j].%s, %s{Row: k})",
			outerField, field, outerField, field, m.RowType)
	} else {
		g.p("\t\t\tout[a.i].%s[a.j].%s = append(out[a.i].%s[a.j].%s, k)",
			outerField, field, outerField, field)
	}
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	_ = innerRow
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

// emitToManyLinkAll is the many-to-many form of a plan's All: parents, link
// rows, then the far side by primary key.
func (g *gen) emitToManyLinkAll(q string, p relPlan) {
	keyField := exportName(p.ParentKey)
	linkParent := exportName(p.LinkParentCol)
	linkChild := exportName(p.LinkChildCol)
	childKey := exportName(p.ChildKey)
	field := exportName(p.rel.Field)

	g.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%sRow, error) {", q, p.Name)
	g.p("\tparents, err := p.q.All(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif len(parents) == 0 {")
	g.p("\t\t// No parents, no link query, no far query. An empty parent set costs")
	g.p("\t\t// ONE round trip, not three.")
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

	g.p("\tlinks, err := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(p.childLimit).All(ctx, ex, nil)",
		p.LinkPkg, p.LinkPkg, linkParent)
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\t// A partial relation load is worse than a failed one: every count")
	g.p("\t// computed from it is wrong and nothing says so. The guard is on the")
	g.p("\t// LINK rows, which is where the fan-out actually is.")
	g.p("\tif int64(len(links)) >= p.childLimit {")
	g.p("\t\treturn nil, runtime.ErrChildLimit")
	g.p("\t}")
	g.p("\tif len(links) == 0 {")
	g.p("\t\treturn out, nil")
	g.p("\t}")

	g.p("\tseen := make(map[%s]bool, len(links))", p.ChildKeyGo)
	g.p("\tfar := make([]%s, 0, len(links))", p.ChildKeyGo)
	g.p("\tfor _, l := range links {")
	g.p("\t\tif !seen[l.%s] {", linkChild)
	g.p("\t\t\tseen[l.%s] = true", linkChild)
	g.p("\t\t\tfar = append(far, l.%s)", linkChild)
	g.p("\t\t}")
	g.p("\t}")

	g.p("\tcq := %s.New().Unordered().Where(%s.%s.In(far...)).Limit(int64(len(far)))",
		p.ChildPkg, p.ChildPkg, childKey)
	g.p("\tif len(p.childOrder) > 0 {")
	g.p("\t\tcq = cq.Order(p.childOrder...)")
	g.p("\t}")
	g.p("\tkids, err := cq.All(ctx, ex, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tbyKey := make(map[%s]%s.Row, len(kids))", p.ChildKeyGo, p.ChildPkg)
	g.p("\tfor _, k := range kids {")
	g.p("\t\tbyKey[k.%s] = k", childKey)
	g.p("\t}")

	g.p("\t// Attached in the order the CHILD query returned, so ChildOrder means")
	g.p("\t// what it says: walking the link rows instead would order by the join")
	g.p("\t// table and silently ignore it.")
	g.p("\tfor _, k := range kids {")
	g.p("\t\tfor _, l := range links {")
	g.p("\t\t\tif l.%s != k.%s {", linkChild, childKey)
	g.p("\t\t\t\tcontinue")
	g.p("\t\t\t}")
	g.p("\t\t\tif i, ok := at[l.%s]; ok {", linkParent)
	g.p("\t\t\t\tout[i].%s = append(out[i].%s, k)", field, field)
	g.p("\t\t\t}")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn out, nil")
	g.p("}")
	g.p("")
}

func (g *gen) emitToManyPlan(p relPlan) {
	parentField := exportName(p.parent.GoName)
	keyField := exportName(p.ParentKey)
	// Meaningless for a many-to-many, where neither side carries a key. Left
	// computed unconditionally they are "", and the emitted code reads
	// `tag..In(...)`, which is what a generator producing unparsable Go looks
	// like from the outside.
	var childKeyField, childHandle string
	if !p.isLink() {
		childKeyField = exportName(p.rel.Column)
		childHandle = childKeyField
	}

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
	if !p.isLink() {
		g.p("func (p %s) ChildTop(n int64) %s {", q, q)
		g.p("\tp.childTop = n")
		g.p("\treturn p")
		g.p("}")
		g.p("")
	}
	g.p("// ChildOrder orders the children within each parent.")
	g.p("//")
	g.p("// Without it they arrive in the child table's default order, which is its")
	g.p("// primary key — defined, but almost never what a caller wanted to show.")
	g.p("func (p %s) ChildOrder(ts ...%s.Sort) %s {", q, p.ChildPkg, q)
	g.p("\tp.childOrder = ts")
	g.p("\treturn p")
	g.p("}")
	g.p("")

	if p.isLink() {
		g.p("// All runs the plan in exactly THREE round trips, whatever the counts.")
		g.p("//")
		g.p("// One more than a direct has-many, and that one is what a join table")
		g.p("// costs: the parents, the link rows, then the far side by primary key.")
		g.p("// Fixed, not per parent.")
		g.p("//")
		g.p("// Two queries rather than one join, deliberately. A join returns the far")
		g.p("// row once per parent referencing it — the same tag repeated across every")
		g.p("// post carrying it — which is the row multiplication a batch loader exists")
		g.p("// to avoid. The second query is bounded by DISTINCT children.")
	} else {
		g.p("// All runs the plan in exactly TWO round trips, whatever the parent count.")
	}
	if !p.isLink() {
		g.p("//")
		g.p("// The mechanism is `= ANY($1)`: one placeholder binds the whole id list, so")
		g.p("// fifty parents and five thousand produce the same SQL and share one")
		g.p("// compiled statement. No join is involved, which is why M3 was never")
		g.p("// actually blocked on join support.")
	}
	if p.isLink() {
		g.emitToManyLinkAll(q, p)
		_ = parentField
		return
	}
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
	g.p("\t\t// Unordered: the rows are bucketed into a map by parent, so a")
	g.p("\t\t// server-side sort is paid and then destroyed. ChildOrder still")
	g.p("\t\t// applies when given — order WITHIN a parent survives bucketing.")
	g.p("\t\tcq := %s.New().Unordered().Where(%s.%s.In(ids...)).Limit(p.childLimit)", p.ChildPkg, p.ChildPkg, childHandle)
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
	g.p("\ttargets, err := %s.New().Unordered().", p.ChildPkg)
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
	g.p("\t\t\treturn nil, fmt.Errorf(\"storm: %%s references a missing %%s row\", %q, %q)", p.parent.Name, p.child.Name)
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
