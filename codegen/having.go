package codegen

import (
	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Filtered semi-joins, composed in the context package.
//
//	rows, err := store.UserHavingPosts(
//	    user.New().Where(user.Status.Eq("active")),
//	    post.PublishedAt.IsNotNull(),
//	).All(ctx, ex)
//
// One statement: the parent's predicates, AND EXISTS(child predicates), the
// parent's ordering and limit. The child predicates are post.Pred values —
// typed by the CHILD's package — and neither table package ever imports the
// other: the context concatenates the two token streams (child columns rebased
// past runtime.ChildColBase), routes fragment lookups by column range, and one
// splice numbers placeholders straight across the boundary.
//
// This is why the semi-join was built on tokens: a filtered EXISTS is ordinary
// predicates plus one wrapper token, so caching, composition and identity all
// come from machinery that already existed.

// emitHaving writes one <Parent>Having<Field> composer per testable relation.
func (g *gen) emitHaving(np havingSpec) {
	q := np.Name + "Query"

	g.p("// %s narrows q to rows with at least one matching %s row — the", np.Name, np.child)
	g.p("// filtered semi-join, in one statement. The child predicates are typed")
	g.p("// by the child's own package; ids and values meet only here.")
	g.p("func %s(q %s.Query, ps ...%s.Pred) %s {", np.Name, np.ParentPkg, np.ChildPkg, q)
	g.p("\treturn %s{q: q, c: %s.New().Unordered().Where(ps...)}", q, np.ChildPkg)
	g.p("}")
	g.p("")
	g.p("type %s struct {", q)
	g.p("\tq %s.Query", np.ParentPkg)
	g.p("\tc %s.Query", np.ChildPkg)
	g.p("}")
	g.p("")

	// The combined lowering: parent everything, child fragments past the base,
	// and the relation's EXISTS header — a constant produced at generate time.
	g.p("var %sLowering = func() runtime.Lowering {", lowerFirst(np.Name))
	g.p("\tlw := %s.Lowering()", np.ParentPkg)
	g.p("\tparentFrag := lw.Frag")
	g.p("\tlw.Frag = func(op, col uint32) runtime.Frag {")
	g.p("\t\tif col >= runtime.ChildColBase {")
	g.p("\t\t\treturn %s.FragOf(op, col-runtime.ChildColBase)", np.ChildPkg)
	g.p("\t\t}")
	g.p("\t\treturn parentFrag(op, col)")
	g.p("\t}")
	g.p("\tlw.Exists = func(uint32) string { return %q }", np.header)
	g.p("\treturn lw")
	g.p("}()")
	g.p("")
	g.p("var %sCache = runtime.NewTreeCache()", lowerFirst(np.Name))
	g.p("")

	g.p("func (h %s) stmt(count bool) (*runtime.Stmt, []runtime.Tok) {", q)
	g.p("\tvar buf [%d]runtime.Tok", 2*(maxToks+maxOrder)+4)
	g.p("\ttoks := h.q.PredToks(buf[:0])")
	g.p("\tparentPreds := len(toks) > 0")
	g.p("\tchild := h.c.PredToks(nil)")
	g.p("\ttoks = runtime.OffsetCols(toks, child, runtime.ChildColBase)")
	g.p("\tarity := uint32(0)")
	g.p("\tif len(child) > 0 {")
	g.p("\t\tarity = 1 // the child stream reduces to one stack entry")
	g.p("\t}")
	g.p("\ttoks = append(toks, runtime.MakeExists(0, arity))")
	g.p("\tif parentPreds {")
	g.p("\t\ttoks = append(toks, runtime.MakeGroup(runtime.KAnd, 2))")
	g.p("\t}")
	g.p("\tif !count {")
	g.p("\t\ttoks = h.q.OrderToks(toks)")
	g.p("\t}")
	g.p("\tsel, cnt, limitSfx := %s.StmtPieces()", np.ParentPkg)
	g.p("\tprefix, suffix := sel, limitSfx")
	g.p("\tif count {")
	g.p("\t\tprefix, suffix = cnt, \"\"")
	g.p("\t}")
	g.p("\tif st := %sCache.Get(toks); st != nil {", lowerFirst(np.Name))
	g.p("\t\treturn st, toks")
	g.p("\t}")
	g.p("\treturn %sCache.Put(toks, runtime.SpliceTree(prefix, toks, %sLowering, suffix)), toks",
		lowerFirst(np.Name), lowerFirst(np.Name))
	g.p("}")
	g.p("")

	g.p("// All runs the composed statement. Bind order is stream order: parent")
	g.p("// values, child values, then the parent's paging.")
	g.p("func (h %s) All(ctx context.Context, ex runtime.Executor) ([]%s.Row, error) {", q, np.ParentPkg)
	g.p("\tif err := h.q.Err(); err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif err := h.c.Err(); err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tst, _ := h.stmt(false)")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn nil, st.Err")
	g.p("\t}")
	g.p("\tpb := %s.GetBinder()", np.ParentPkg)
	g.p("\tdefer %s.PutBinder(pb)", np.ParentPkg)
	g.p("\tcb := %s.GetBinder()", np.ChildPkg)
	g.p("\tdefer %s.PutBinder(cb)", np.ChildPkg)
	g.p("\targs := h.q.BindPreds(pb, nil)")
	g.p("\targs = h.c.BindPreds(cb, args)")
	g.p("\targs = h.q.BindPaging(pb, args)")
	g.p("\trows, err := ex.Query(ctx, st.SQL, args)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tvar sl runtime.Slab")
	g.p("\tvar out []%s.Row", np.ParentPkg)
	g.p("\tfor rows.Next() {")
	g.p("\t\tout = append(out, %s.Row{})", np.ParentPkg)
	g.p("\t\tif err := %s.Scan(rows.RawValues(), &out[len(out)-1], &sl); err != nil {", np.ParentPkg)
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn out, rows.Err()")
	g.p("}")
	g.p("")

	g.p("// Count runs the composed count: no ordering, no paging.")
	g.p("func (h %s) Count(ctx context.Context, ex runtime.Executor) (int64, error) {", q)
	g.p("\tif err := h.q.Err(); err != nil {")
	g.p("\t\treturn 0, err")
	g.p("\t}")
	g.p("\tif err := h.c.Err(); err != nil {")
	g.p("\t\treturn 0, err")
	g.p("\t}")
	g.p("\tst, _ := h.stmt(true)")
	g.p("\tif st.Err != nil {")
	g.p("\t\treturn 0, st.Err")
	g.p("\t}")
	g.p("\tpb := %s.GetBinder()", np.ParentPkg)
	g.p("\tdefer %s.PutBinder(pb)", np.ParentPkg)
	g.p("\tcb := %s.GetBinder()", np.ChildPkg)
	g.p("\tdefer %s.PutBinder(cb)", np.ChildPkg)
	g.p("\targs := h.q.BindPreds(pb, nil)")
	g.p("\targs = h.c.BindPreds(cb, args)")
	g.p("\trows, err := ex.Query(ctx, st.SQL, args)")
	g.p("\tif err != nil {")
	g.p("\t\treturn 0, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tif !rows.Next() {")
	g.p("\t\treturn 0, rows.Err()")
	g.p("\t}")
	g.p("\treturn runtime.Int8(rows.RawValues()[0]), rows.Err()")
	g.p("}")
	g.p("")
}

// havingSpec is one composer to emit.
type havingSpec struct {
	Name      string // UserHavingPosts
	ParentPkg string
	ChildPkg  string
	child     string // child table name, for docs
	header    string // the EXISTS opener, built by pgsql at generate time
}

// havingSpecs enumerates the composers for a context.
func havingSpecs(s *schema.Schema, tables []string) ([]havingSpec, error) {
	in := map[string]bool{}
	for _, t := range tables {
		in[t] = true
	}
	var out []havingSpec
	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		for _, rel := range existsRelations(t) {
			if !in[rel.Target] {
				continue
			}
			child := s.Table(rel.Target)
			ppkg, err := PackageName(t.GoName, t.Name)
			if err != nil {
				return nil, err
			}
			cpkg, err := PackageName(child.GoName, child.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, havingSpec{
				Name:      exportName(t.GoName) + "Having" + exportName(rel.Field),
				ParentPkg: ppkg,
				ChildPkg:  cpkg,
				child:     child.Name,
				header:    pgsql.ExistsOpen(child.Name, rel.Column, t.Name, t.PrimaryKey[0]),
			})
		}
	}
	return out, nil
}
