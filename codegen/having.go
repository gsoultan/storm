package codegen

import (
	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
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

// emitHaving writes one composer per testable relation: the semi-join, the
// anti-join, and the chain that combines them.
//
// The two share a TYPE, because they share a statement. "Bought Coffee but
// never Equipment" is one EXISTS and one NOT EXISTS against the same relation,
// and doing it in two round trips means intersecting in Go — which is the join
// the database was going to do anyway, done worse.
//
// Chaining is per RELATION, enforced by the type. Two probes against different
// relations would rebase both children past the same runtime.ChildColBase and
// the composite lowering routes on that range alone, so it could not tell one
// child package's fragments from the other's. A method that does not exist is
// a better answer than a wrong row.
func (g *gen) emitHaving(np havingSpec) {
	q := np.TypeName

	g.p("// %s is a parent query with one or more existence probes against", q)
	g.p("// %s, combined with AND. Build it with %sHaving%s or", np.child, np.Parent, np.Rel)
	g.p("// %sNotHaving%s and extend it with AndHaving/AndNotHaving.", np.Parent, np.Rel)
	g.p("type %s struct {", q)
	g.p("	q %s.Query", np.ParentPkg)
	g.p("	p []%sProbe", lowerFirst(q))
	g.p("}")
	g.p("")
	g.p("type %sProbe struct {", lowerFirst(q))
	g.p("	c   %s.Query", np.ChildPkg)
	g.p("	neg bool")
	g.p("}")
	g.p("")

	g.p("// %sHaving%s narrows q to rows with at least one matching %s row —", np.Parent, np.Rel, np.child)
	g.p("// the filtered semi-join, in one statement. The child predicates are")
	g.p("// typed by the child's own package; ids and values meet only here.")
	g.p("func %sHaving%s(q %s.Query, ps ...%s.Pred) %s {", np.Parent, np.Rel, np.ParentPkg, np.ChildPkg, q)
	g.p("	return %s{q: q}.AndHaving(ps...)", q)
	g.p("}")
	g.p("")

	g.p("// %sNotHaving%s narrows q to rows with NO matching %s row — the", np.Parent, np.Rel, np.child)
	g.p("// filtered anti-join.")
	g.p("//")
	g.p("// Read the predicates carefully: this is \"has no %s row matching", np.child)
	g.p("// these\", not \"has a %s row that does not match\". With no predicates", np.child)
	g.p("// at all it is \"has none\". The two questions have different answers")
	g.p("// whenever a parent has several children, and SQL spells them the same")
	g.p("// way round.")
	g.p("func %sNotHaving%s(q %s.Query, ps ...%s.Pred) %s {", np.Parent, np.Rel, np.ParentPkg, np.ChildPkg, q)
	g.p("	return %s{q: q}.AndNotHaving(ps...)", q)
	g.p("}")
	g.p("")

	g.p("// AndHaving adds another EXISTS probe against %s, ANDed with the", np.child)
	g.p("// ones already there.")
	g.p("func (h %s) AndHaving(ps ...%s.Pred) %s {", q, np.ChildPkg, q)
	g.p("	return h.probe(false, ps...)")
	g.p("}")
	g.p("")
	g.p("// AndNotHaving adds a NOT EXISTS probe against %s. This is how", np.child)
	g.p("// \"has one of these but none of those\" is one statement:")
	g.p("//")
	g.p("//\t%sHaving%s(q, bought).AndNotHaving(alsoBought)", np.Parent, np.Rel)
	g.p("func (h %s) AndNotHaving(ps ...%s.Pred) %s {", q, np.ChildPkg, q)
	g.p("	return h.probe(true, ps...)")
	g.p("}")
	g.p("")
	g.p("func (h %s) probe(neg bool, ps ...%s.Pred) %s {", q, np.ChildPkg, q)
	g.p("	// Copied rather than appended in place: a composer is a value, and")
	g.p("	// two chains branching from one base must not share a backing array.")
	g.p("	p := make([]%sProbe, len(h.p), len(h.p)+1)", lowerFirst(q))
	g.p("	copy(p, h.p)")
	g.p("	h.p = append(p, %sProbe{c: %s.New().Unordered().Where(ps...), neg: neg})", lowerFirst(q), np.ChildPkg)
	g.p("	return h")
	g.p("}")
	g.p("")

	// The combined lowering: parent everything, child fragments past the base,
	// and the relation's EXISTS header selected by the token's relation id.
	g.p("var %sLowering = func() runtime.Lowering {", lowerFirst(q))
	g.p("	lw := %s.Lowering()", np.ParentPkg)
	g.p("	parentFrag := lw.Frag")
	g.p("	lw.Frag = func(op, col uint32) runtime.Frag {")
	g.p("		if col >= runtime.ChildColBase {")
	g.p("			return %s.FragOf(op, col-runtime.ChildColBase)", np.ChildPkg)
	g.p("		}")
	g.p("		return parentFrag(op, col)")
	g.p("	}")
	g.p("	// The token's relation id picks the header: 0 positive, 1 negated.")
	g.p("	// Both probes are the same relation, so only the polarity varies.")
	g.p("	lw.Exists = func(rel uint32) string {")
	g.p("		if rel == 1 {")
	g.p("			return %q", np.NegHeader)
	g.p("		}")
	g.p("		return %q", np.PosHeader)
	g.p("	}")
	g.p("	return lw")
	g.p("}()")
	g.p("")
	g.p("var %sCache = runtime.NewTreeCache()", lowerFirst(q))
	g.p("")

	g.p("func (h %s) stmt(count bool) (*runtime.Stmt, []runtime.Tok) {", q)
	g.p("	var buf [%d]runtime.Tok", 2*(maxToks+maxOrder)+4)
	g.p("	toks := h.q.PredToks(buf[:0])")
	g.p("	stack := 0")
	g.p("	if len(toks) > 0 {")
	g.p("		stack = 1")
	g.p("	}")
	g.p("	for _, pr := range h.p {")
	g.p("		child := pr.c.PredToks(nil)")
	g.p("		toks = runtime.OffsetCols(toks, child, runtime.ChildColBase)")
	g.p("		arity := uint32(0)")
	g.p("		if len(child) > 0 {")
	g.p("			arity = 1 // the child stream reduces to one stack entry")
	g.p("		}")
	g.p("		rel := uint32(0)")
	g.p("		if pr.neg {")
	g.p("			rel = 1")
	g.p("		}")
	g.p("		toks = append(toks, runtime.MakeExists(rel, arity))")
	g.p("		stack++")
	g.p("	}")
	g.p("	if stack > 1 {")
	g.p("		toks = append(toks, runtime.MakeGroup(runtime.KAnd, uint32(stack)))")
	g.p("	}")
	g.p("	if !count {")
	g.p("		toks = h.q.OrderToks(toks)")
	g.p("	}")
	g.p("	sel, cnt, limitSfx := %s.StmtPieces()", np.ParentPkg)
	g.p("	prefix, suffix := sel, limitSfx")
	g.p("	if count {")
	g.p("		prefix, suffix = cnt, \"\"")
	g.p("	}")
	g.p("	if st := %sCache.Get(toks); st != nil {", lowerFirst(q))
	g.p("		return st, toks")
	g.p("	}")
	g.p("	return %sCache.Put(toks, runtime.SpliceTree(prefix, toks, %sLowering, suffix)), toks",
		lowerFirst(q), lowerFirst(q))
	g.p("}")
	g.p("")

	g.p("// bind returns the arguments in STREAM order: parent predicates, then")
	g.p("// each probe's, then the parent's paging. Each probe takes its own")
	g.p("// binder — bindPreds resets the arenas it fills, so two probes sharing")
	g.p("// one binder would have the second silently overwrite the first.")
	g.p("func (h %s) bind(pb *%s.Binder, paging bool) ([]any, []*%s.Binder) {", q, np.ParentPkg, np.ChildPkg)
	g.p("	args := h.q.BindPreds(pb, nil)")
	g.p("	cbs := make([]*%s.Binder, 0, len(h.p))", np.ChildPkg)
	g.p("	for _, pr := range h.p {")
	g.p("		cb := %s.GetBinder()", np.ChildPkg)
	g.p("		cbs = append(cbs, cb)")
	g.p("		args = pr.c.BindPreds(cb, args)")
	g.p("	}")
	g.p("	if paging {")
	g.p("		args = h.q.BindPaging(pb, args)")
	g.p("	}")
	g.p("	return args, cbs")
	g.p("}")
	g.p("")
	g.p("func (h %s) err() error {", q)
	g.p("	if err := h.q.Err(); err != nil {")
	g.p("		return err")
	g.p("	}")
	g.p("	for _, pr := range h.p {")
	g.p("		if err := pr.c.Err(); err != nil {")
	g.p("			return err")
	g.p("		}")
	g.p("	}")
	g.p("	return nil")
	g.p("}")
	g.p("")

	g.p("// All runs the composed statement.")
	g.p("func (h %s) All(ctx context.Context, ex runtime.Executor) ([]%s.Row, error) {", q, np.ParentPkg)
	g.p("	if err := h.err(); err != nil {")
	g.p("		return nil, err")
	g.p("	}")
	g.p("	st, _ := h.stmt(false)")
	g.p("	if st.Err != nil {")
	g.p("		return nil, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	args, cbs := h.bind(pb, true)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	rows, err := ex.Query(ctx, st.SQL, args)")
	g.p("	if err != nil {")
	g.p("		return nil, err")
	g.p("	}")
	g.p("	defer rows.Close()")
	g.p("	var sl runtime.Slab")
	g.p("	var out []%s.Row", np.ParentPkg)
	g.p("	for rows.Next() {")
	g.p("		out = append(out, %s.Row{})", np.ParentPkg)
	g.p("		if err := %s.Scan(rows.RawValues(), &out[len(out)-1], &sl); err != nil {", np.ParentPkg)
	g.p("			return nil, err")
	g.p("		}")
	g.p("	}")
	g.p("	return out, rows.Err()")
	g.p("}")
	g.p("")

	g.p("// Count runs the composed count: no ordering, no paging.")
	g.p("func (h %s) Count(ctx context.Context, ex runtime.Executor) (int64, error) {", q)
	g.p("	if err := h.err(); err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	st, _ := h.stmt(true)")
	g.p("	if st.Err != nil {")
	g.p("		return 0, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	args, cbs := h.bind(pb, false)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	rows, err := ex.Query(ctx, st.SQL, args)")
	g.p("	if err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	defer rows.Close()")
	g.p("	if !rows.Next() {")
	g.p("		return 0, rows.Err()")
	g.p("	}")
	g.p("	return runtime.Int8(rows.RawValues()[0]), rows.Err()")
	g.p("}")
	g.p("")
}

// havingSpec is one composer to emit.
type havingSpec struct {
	Parent    string // User
	Rel       string // Posts
	TypeName  string // UserPostsProbeQuery
	ParentPkg string
	ChildPkg  string
	child     string // child table name, for docs
	PosHeader string // the EXISTS opener, built by pgsql at generate time
	NegHeader string // the NOT EXISTS opener
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
			relColumn := rel.Column
			child := s.Table(rel.Target)
			ppkg, err := PackageName(t.GoName, t.Name)
			if err != nil {
				return nil, err
			}
			cpkg, err := PackageName(child.GoName, child.Name)
			if err != nil {
				return nil, err
			}
			parent := exportName(t.GoName)
			rel := exportName(rel.Field)
			out = append(out, havingSpec{
				Parent:    parent,
				Rel:       rel,
				TypeName:  parent + rel + "ProbeQuery",
				ParentPkg: ppkg,
				ChildPkg:  cpkg,
				child:     child.Name,
				PosHeader: pgsql.ExistsOpen(child.Name, relColumn, t.Name, t.PrimaryKey[0]),
				NegHeader: pgsql.NotExistsOpen(child.Name, relColumn, t.Name, t.PrimaryKey[0]),
			})
		}
	}
	return out, nil
}
