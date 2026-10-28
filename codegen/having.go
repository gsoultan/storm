package codegen

import (
	"fmt"

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
	g.p("	var buf [%d]runtime.Tok", g.composedBuf())
	g.p("	toks := h.tokens(buf[:0], count)")
	g.p("	sel, cnt, limitSfx := %s.StmtPieces()", np.ParentPkg)
	g.p("	prefix, suffix := sel, limitSfx")
	g.p("	if count {")
	g.p("		prefix, suffix = cnt, \"\"")
	g.p("	}")
	g.p("	if st := %sCache.Get(toks); st != nil {", lowerFirst(q))
	g.p("		return st, toks")
	g.p("	}")
	// The parent's marked rows stay out, as they do from every read of its
	// own package: a probe narrows the parent query, it does not widen it.
	if np.ParentLive != "" {
		g.p("	return %sCache.Put(toks, runtime.SpliceTreeWhere(prefix, %s, toks, %sLowering, suffix)), toks",
			lowerFirst(q), lit(np.ParentLive), lowerFirst(q))
	} else {
		g.p("	return %sCache.Put(toks, runtime.SpliceTree(prefix, toks, %sLowering, suffix)), toks",
			lowerFirst(q), lowerFirst(q))
	}
	g.p("}")
	g.p("")

	g.p("// tokens appends the composed stream to dst: the parent's predicates,")
	g.p("// each probe, and — unless count — the parent's ordering.")
	g.p("func (h %s) tokens(dst []runtime.Tok, count bool) []runtime.Tok {", q)
	g.p("	toks := h.q.PredToks(dst)")
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
	g.p("	return toks")
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
	g.p("func (h %s) All(ctx context.Context, ex "+g.execType()+") ([]%s.Row, error) {", q, np.ParentPkg)
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
	g.p("	rows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("	if err != nil {")
	g.p("		return nil, err")
	g.p("	}")
	g.p("	defer rows.Close()")
	g.p("	var sl runtime.Slab")
	g.p("	var out []%s.Row", np.ParentPkg)
	g.p("	for rows.Next() {")
	g.p("		out = append(out, %s.Row{})", np.ParentPkg)
	g.p("		if err := %s.Scan(rows.%s(), &out[len(out)-1], &sl); err != nil {", np.ParentPkg, g.dec.rowsAccessor())
	g.p("			return nil, err")
	g.p("		}")
	g.p("	}")
	g.p("	return out, rows.Err()")
	g.p("}")
	g.p("")

	g.p("// Count runs the composed count: no ordering, no paging.")
	g.p("func (h %s) Count(ctx context.Context, ex "+g.execType()+") (int64, error) {", q)
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
	g.p("	rows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("	if err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	defer rows.Close()")
	g.p("	if !rows.Next() {")
	g.p("		return 0, rows.Err()")
	g.p("	}")
	g.p("\treturn %s(rows.%s()[0]), rows.Err()", g.dec.q("Int8"), g.dec.rowsAccessor())
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
	// ParentLive is the parent's own soft-delete predicate, or empty. The
	// child's is in the headers; the parent's goes ahead of the caller's
	// predicates, as every read of the parent's own package puts it.
	ParentLive string
	// ParentTable is the parent's table, which an INSERT … SELECT whose
	// source it is names.
	ParentTable string
}

// havingSpecs enumerates the composers for a context.
//
// lw is the target's lowering, not PostgreSQL's. It was PostgreSQL's, and the
// semi-join in a MySQL package came out with double-quoted identifiers — SQL
// that parses nowhere. The context file is generated once per package rather
// than per table, which is how it kept a hard-coded dialect while everything
// generated per table went through the seam.
func havingSpecs(lw lowering, s *schema.Schema, tables []string) ([]havingSpec, error) {
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
				PosHeader: lw.ExistsOpen(child.Name, relColumn, t.Name, t.PrimaryKey[0],
					string(liveIn(lw, s, child.Name, ""))),
				NegHeader: lw.NotExistsOpen(child.Name, relColumn, t.Name, t.PrimaryKey[0],
					string(liveIn(lw, s, child.Name, ""))),
				ParentLive:  string(liveIn(lw, s, t.Name, "")),
				ParentTable: t.Name,
			})
		}
		for _, field := range t.Probes {
			spec, err := toOneProbe(lw, s, t, field)
			if err != nil {
				return nil, err
			}
			if in[spec.child] {
				out = append(out, spec)
			}
		}
		for _, m := range t.Matches {
			spec, err := matchProbe(lw, s, t, m)
			if err != nil {
				return nil, err
			}
			if in[spec.child] {
				out = append(out, spec)
			}
		}
	}
	return out, nil
}

// matchProbe is the composer of a declared probe on matching columns
// (t.ProbesOn): the EXISTS correlated on every pair rather than a key.
func matchProbe(lw lowering, s *schema.Schema, t *schema.Table, m *schema.Match) (havingSpec, error) {
	if lw.ExistsOnOpen == nil {
		return havingSpec{}, fmt.Errorf("codegen: table %s: ProbesOn %s: this back end has no multi-column probe yet", t.Name, m.Name)
	}
	target := s.Table(m.Target)
	if target == nil {
		return havingSpec{}, fmt.Errorf("codegen: table %s: ProbesOn %s names %s, which is not in the schema", t.Name, m.Name, m.Target)
	}
	ppkg, err := PackageName(t.GoName, t.Name)
	if err != nil {
		return havingSpec{}, err
	}
	cpkg, err := PackageName(target.GoName, target.Name)
	if err != nil {
		return havingSpec{}, err
	}
	live := string(liveIn(lw, s, target.Name, ""))
	parent := exportName(t.GoName)
	return havingSpec{
		Parent:      parent,
		Rel:         m.Name,
		TypeName:    parent + m.Name + "ProbeQuery",
		ParentPkg:   ppkg,
		ChildPkg:    cpkg,
		child:       target.Name,
		PosHeader:   lw.ExistsOnOpen(target.Name, m.TargetColumns, t.Name, m.Columns, live),
		NegHeader:   lw.NotExistsOnOpen(target.Name, m.TargetColumns, t.Name, m.Columns, live),
		ParentLive:  string(liveIn(lw, s, t.Name, "")),
		ParentTable: t.Name,
	}, nil
}

// toOneProbe is the composer through an owned to-one reference: the same
// EXISTS, correlated the other way round. The referenced row's key equals
// this row's foreign key, where a to-many's child key equals this row's key.
func toOneProbe(lw lowering, s *schema.Schema, t *schema.Table, field string) (havingSpec, error) {
	var rel *schema.Relation
	for _, r := range t.Relations {
		if r.Field == field {
			rel = r
		}
	}
	if rel == nil || !rel.Owner || rel.ToMany || rel.Column == "" || rel.Link != "" {
		return havingSpec{}, fmt.Errorf("codegen: table %s: Probes names %s, and only an owned to-one reference can be probed that way", t.Name, field)
	}
	target := s.Table(rel.Target)
	if target == nil || len(target.PrimaryKey) != 1 {
		return havingSpec{}, fmt.Errorf("codegen: table %s: Probes names %s, whose table has no one-column key to correlate on", t.Name, field)
	}
	ppkg, err := PackageName(t.GoName, t.Name)
	if err != nil {
		return havingSpec{}, err
	}
	cpkg, err := PackageName(target.GoName, target.Name)
	if err != nil {
		return havingSpec{}, err
	}
	live := string(liveIn(lw, s, target.Name, ""))
	parent, name := exportName(t.GoName), exportName(rel.Field)
	return havingSpec{
		Parent:      parent,
		Rel:         name,
		TypeName:    parent + name + "ProbeQuery",
		ParentPkg:   ppkg,
		ChildPkg:    cpkg,
		child:       target.Name,
		PosHeader:   lw.ExistsOpen(target.Name, target.PrimaryKey[0], t.Name, rel.Column, live),
		NegHeader:   lw.NotExistsOpen(target.Name, target.PrimaryKey[0], t.Name, rel.Column, live),
		ParentLive:  string(liveIn(lw, s, t.Name, "")),
		ParentTable: t.Name,
	}, nil
}

// INSERT … SELECT, composed in the context package as the semi-join is.
//
//	var ins item.Ins
//	ins.SetBatchID(batch)
//	ins.SetStatus("pending")
//	n, err := store.InsertItemPerInstance(ctx, ex, ins,
//	    instance.New().Where(instance.ProjectID.Eq(p)).Limit(10_000))
//
// One statement: one item row per instance the query selects, the reference
// filled from the instance's key and every other assigned column carried
// alike. Nothing is read into Go. The target's package knows its columns and
// values (Ins.SelectHead), the source's its predicates, order and limit; the
// splice numbers the read's placeholders on from the head's.

// insertSelectFor names the pieces of one INSERT … SELECT: a reference rel of
// table t filled from the rows a query of rel's target selects. fn is its
// name, ref the reference column's insert bit, and splice the call that
// splices the read onto the head, given the read's lowering.
func insertSelectFor(lw lowering, s *schema.Schema, t *schema.Table, rel *schema.Relation) (
	fn, tpkg, spkg string, ref int, splice func(lowering string) string, err error,
) {
	src := s.Table(rel.Target)
	if tpkg, err = PackageName(t.GoName, t.Name); err != nil {
		return
	}
	if spkg, err = PackageName(src.GoName, src.Name); err != nil {
		return
	}
	fn = "Insert" + exportName(t.GoName) + "Per" + exportName(rel.Field)
	ref = insertIndex(insertable(t), rel.Column)
	keyFrom := lit(lw.KeyFrom(src.Name, src.PrimaryKey[0]))
	live := string(liveIn(lw, s, src.Name, ""))
	splice = func(lowering string) string {
		if live != "" {
			return fmt.Sprintf("runtime.SpliceTreeWhereFrom(head.SQL+%s, %s, toks, %s, limitSfx, head.NArg)",
				keyFrom, lit(live), lowering)
		}
		return fmt.Sprintf("runtime.SpliceTreeFrom(head.SQL+%s, toks, %s, limitSfx, head.NArg)", keyFrom, lowering)
	}
	return
}

// insertSelects calls each for every INSERT … SELECT among tables whose
// source is also among them.
func insertSelects(lw lowering, s *schema.Schema, tables []string, each func(t *schema.Table, rel *schema.Relation) error) error {
	in := map[string]bool{}
	for _, t := range tables {
		in[t] = true
	}
	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		for _, rel := range insertSelectRefs(lw, s, t) {
			if !in[rel.Target] {
				continue
			}
			if err := each(t, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitInsertSelect writes the function over a plain query of the source.
func (g *gen) emitInsertSelect(lw lowering, s *schema.Schema, t *schema.Table, rel *schema.Relation) error {
	fn, tpkg, spkg, ref, splice, err := insertSelectFor(lw, s, t, rel)
	if err != nil {
		return err
	}
	cache := lowerFirst(fn) + "Cache"
	g.p("var %s = runtime.NewTreeCache()", cache)
	g.p("")
	g.p("// %s inserts one %s row per row q selects — its", fn, t.Name)
	g.p("// predicates, order and Limit apply — with %s set to that row's key", rel.Column)
	g.p("// and every other column assigned on ins carried alike, in ONE")
	g.p("// statement, and reports how many it inserted. Nothing is read into Go.")
	g.p("// A query holding a row lock is refused with runtime.ErrPagedWrite, and so")
	g.p("// is an Offset: neither has a place in the statement.")
	g.p("func %s(ctx context.Context, ex %s, ins %s.Ins, q %s.Query) (int64, error) {", fn, g.execType(), tpkg, spkg)
	g.p("	if err := q.Err(); err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	if q.Locked() || q.Paged() {")
	g.p("		return 0, runtime.ErrPagedWrite")
	g.p("	}")
	g.p("	var buf [%d]runtime.Tok", g.composedBuf())
	g.p("	toks := q.OrderToks(q.PredToks(buf[:0]))")
	g.p("	head, args := ins.SelectHead(%d)", ref)
	g.p("	if head.Err != nil {")
	g.p("		return 0, head.Err")
	g.p("	}")
	g.p("	mask := ins.Assigned() &^ (1 << %d)", ref)
	g.p("	var key [%d]runtime.Tok", g.composedBuf()+2)
	g.p("	key[0], key[1] = runtime.Tok(mask), runtime.Tok(mask>>32)")
	g.p("	n := 2 + copy(key[2:], toks)")
	g.p("	st := %s.Get(key[:n])", cache)
	g.p("	if st == nil {")
	g.p("		_, _, limitSfx := %s.StmtPieces()", spkg)
	g.p("		st = %s.Put(key[:n], %s)", cache, splice(spkg+".Lowering()"))
	g.p("	}")
	g.p("	if st.Err != nil {")
	g.p("		return 0, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", spkg)
	g.p("	defer %s.PutBinder(pb)", spkg)
	g.p("	args = q.BindPaging(pb, q.BindPreds(pb, args))")
	g.p("	return ex.Exec(ctx, st.SQL, args)")
	g.p("}")
	g.p("")
	return nil
}

// emitProbeDeleteBatch writes DeleteBatch over a semi-join of the parent:
// the bounded purge of the parent rows the probes select. HardDeleteBatch on
// a soft-delete parent, which reads its marked rows too, as the table's own
// does. Only where the parent asks for Take and the back end can spell it.
func (g *gen) emitProbeDeleteBatch(np havingSpec, lw lowering, s *schema.Schema) {
	parent := s.Table(np.ParentTable)
	if parent == nil || !parent.Takes || lw.TrimOpen == nil {
		return
	}
	name := "DeleteBatch"
	if parent.SoftDeletes() {
		name = "HardDeleteBatch"
	}
	q := np.TypeName
	cache := lowerFirst(q) + name + "Cache"
	g.p("var %s = runtime.NewTreeCache()", cache)
	g.p("")
	g.p("// %s is the %s package's method of that name over this", name, np.ParentPkg)
	g.p("// semi-join: it removes the %s rows the composed read selects —", np.ParentTable)
	g.p("// its predicates, probes, order and Limit — in ONE statement, and")
	g.p("// reports how many. A row lock or an Offset is refused with")
	g.p("// runtime.ErrPagedWrite.")
	g.p("func (h %s) %s(ctx context.Context, ex %s) (int64, error) {", q, name, g.execType())
	g.p("	if err := h.err(); err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	if h.q.Locked() || h.q.Paged() {")
	g.p("		return 0, runtime.ErrPagedWrite")
	g.p("	}")
	g.p("	var buf [%d]runtime.Tok", g.composedBuf())
	g.p("	toks := h.tokens(buf[:0], false)")
	g.p("	st := %s.Get(toks)", cache)
	g.p("	if st == nil {")
	g.p("		_, _, limitSfx := %s.StmtPieces()", np.ParentPkg)
	g.p("		st = %s.Put(toks, runtime.SpliceTree(%s, toks, %sLowering, limitSfx+%s))",
		cache, lit(lw.DeletePrefix(parent.Name)+lw.TrimOpen(parent.Name)), lowerFirst(q), lit(lw.TrimClose))
	g.p("	}")
	g.p("	if st.Err != nil {")
	g.p("		return 0, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	args, cbs := h.bind(pb, true)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	return ex.Exec(ctx, st.SQL, args)")
	g.p("}")
	g.p("")
}

// emitProbeTake writes Take over a semi-join: the parent package's claim,
// narrowed by the composed read instead of its own. The parent spells the
// UPDATE and the lock (Mut.TakeHead); the composer splices its stream in
// between, so the probe, the order, the Limit and the row lock all land in
// the one subquery the UPDATE is bounded by.
func (g *gen) emitProbeTake(np havingSpec, lw lowering, s *schema.Schema) {
	parent := s.Table(np.ParentTable)
	if parent == nil || !takes(lw, parent) {
		return
	}
	q := np.TypeName
	cache := lowerFirst(q) + "TakeCache"
	g.p("var %s = runtime.NewTreeCache()", cache)
	g.p("")
	g.p("// Take is the %s package's Take over this semi-join: it writes m's", np.ParentPkg)
	g.p("// assigned columns to the %s rows the composed read selects — its", np.ParentTable)
	g.p("// predicates, probes, order, Limit and row lock — in ONE statement, and")
	g.p("// returns them as they now stand. Nothing assigned issues no statement")
	g.p("// and returns dst; an Offset is refused with runtime.ErrPagedWrite.")
	g.p("func (h %s) Take(ctx context.Context, ex %s, m %s.Mut, dst []%s.Row) ([]%s.Row, error) {",
		q, g.execType(), np.ParentPkg, np.ParentPkg, np.ParentPkg)
	g.p("	if err := h.err(); err != nil {")
	g.p("		return dst, err")
	g.p("	}")
	g.p("	if h.q.Paged() {")
	g.p("		return dst, runtime.ErrPagedWrite")
	g.p("	}")
	g.p("	head, open, close, key, args, ok := m.TakeHead(h.q)")
	g.p("	if !ok {")
	g.p("		return dst, nil")
	g.p("	}")
	g.p("	var buf [%d]runtime.Tok", g.composedBuf()+5)
	g.p("	toks := h.tokens(append(buf[:0], key[:]...), false)")
	g.p("	st := %s.Get(toks)", cache)
	g.p("	if st == nil {")
	g.p("		if head.Err != nil {")
	g.p("			return dst, head.Err")
	g.p("		}")
	g.p("		_, _, limitSfx := %s.StmtPieces()", np.ParentPkg)
	if np.ParentLive != "" {
		g.p("		st = %s.Put(toks, runtime.SpliceTreeWhereFrom(head.SQL+open, %s, toks[len(key):], %sLowering, limitSfx+close, head.NArg))",
			cache, lit(np.ParentLive), lowerFirst(q))
	} else {
		g.p("		st = %s.Put(toks, runtime.SpliceTreeFrom(head.SQL+open, toks[len(key):], %sLowering, limitSfx+close, head.NArg))",
			cache, lowerFirst(q))
	}
	g.p("	}")
	g.p("	if st.Err != nil {")
	g.p("		return dst, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	read, cbs := h.bind(pb, true)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	args = append(args, read...)")
	g.p("	rows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
	g.p("	if err != nil {")
	g.p("		return dst, err")
	g.p("	}")
	g.p("	defer rows.Close()")
	g.p("	var sl runtime.Slab")
	g.p("	for rows.Next() {")
	g.p("		dst = append(dst, %s.Row{})", np.ParentPkg)
	g.p("		if err := %s.Scan(rows.%s(), &dst[len(dst)-1], &sl); err != nil {", np.ParentPkg, g.dec.rowsAccessor())
	g.p("			return dst, err")
	g.p("		}")
	g.p("	}")
	g.p("	return dst, rows.Err()")
	g.p("}")
	g.p("")
}

// emitProbeUpdateAll writes UpdateAll over a semi-join: the parent package's
// set-based write, bounded by the composed predicates and probes instead of
// its own. "Put back on offer every task with no open incident at its step"
// is one of these.
func (g *gen) emitProbeUpdateAll(np havingSpec, s *schema.Schema) {
	parent := s.Table(np.ParentTable)
	if parent == nil || len(parent.PrimaryKey) == 0 {
		return
	}
	q := np.TypeName
	cache := lowerFirst(q) + "UpdateAllCache"
	g.p("var %s = runtime.NewTreeCache()", cache)
	g.p("")
	g.p("// UpdateAll is the %s package's UpdateAll over this semi-join: it", np.ParentPkg)
	g.p("// writes m's assigned columns to every %s row the predicates and", np.ParentTable)
	g.p("// probes select, in ONE statement, and reports how many. Nothing")
	g.p("// assigned issues no statement; a Limit, Offset or row lock is")
	g.p("// runtime.ErrPagedWrite.")
	g.p("func (h %s) UpdateAll(ctx context.Context, ex %s, m %s.Mut) (int64, error) {", q, g.execType(), np.ParentPkg)
	g.p("	if err := h.err(); err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	if h.q.Shaped() {")
	g.p("		return 0, runtime.ErrPagedWrite")
	g.p("	}")
	g.p("	head, key, args, ok := m.UpdateHead()")
	g.p("	if !ok {")
	g.p("		return 0, nil")
	g.p("	}")
	g.p("	var buf [%d]runtime.Tok", g.composedBuf()+4)
	g.p("	toks := h.tokens(append(buf[:0], key[:]...), true)")
	g.p("	st := %s.Get(toks)", cache)
	g.p("	if st == nil {")
	g.p("		if head.Err != nil {")
	g.p("			return 0, head.Err")
	g.p("		}")
	if np.ParentLive != "" {
		g.p("		st = %s.Put(toks, runtime.SpliceTreeWhereFrom(head.SQL, %s, toks[len(key):], %sLowering, \"\", head.NArg))",
			cache, lit(np.ParentLive), lowerFirst(q))
	} else {
		g.p("		st = %s.Put(toks, runtime.SpliceTreeFrom(head.SQL, toks[len(key):], %sLowering, \"\", head.NArg))",
			cache, lowerFirst(q))
	}
	g.p("	}")
	g.p("	if st.Err != nil {")
	g.p("		return 0, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	read, cbs := h.bind(pb, false)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	return ex.Exec(ctx, st.SQL, append(args, read...))")
	g.p("}")
	g.p("")
}

// emitProbeAggregates writes, for each of the parent's declared aggregations
// that takes no parameter, the same grouped read over the semi-join: the
// rows that go into the groups are the ones the composed read selects.
// "Open incidents per instance, of the instances this caller may see" is one
// of these.
func (g *gen) emitProbeAggregates(np havingSpec, s *schema.Schema) {
	parent := s.Table(np.ParentTable)
	if parent == nil {
		return
	}
	q := np.TypeName
	for _, agg := range parent.Aggregates {
		if len(agg.Params) > 0 {
			continue
		}
		name := agg.Name
		cache := lowerFirst(q) + name + "Cache"
		g.p("var %s = runtime.NewTreeCache()", cache)
		g.p("")
		g.p("// All%s is the %s package's aggregation of that name over this", name, np.ParentPkg)
		g.p("// semi-join: the predicates and probes filter the rows that go into")
		g.p("// the groups, and Limit pages the groups. An Order, a row lock or an")
		g.p("// Offset is refused, as on the table's own.")
		g.p("func (h %s) All%s(ctx context.Context, ex %s) ([]%s.%sRow, error) {", q, name, g.execType(), np.ParentPkg, name)
		g.p("	if err := h.err(); err != nil {")
		g.p("		return nil, err")
		g.p("	}")
		g.p("	if h.q.Ordered() || h.q.Locked() || h.q.Paged() {")
		g.p("		return nil, runtime.ErrGroupedRead")
		g.p("	}")
		g.p("	var buf [%d]runtime.Tok", g.composedBuf())
		g.p("	toks := h.tokens(buf[:0], true)")
		g.p("	st := %s.Get(toks)", cache)
		g.p("	if st == nil {")
		g.p("		prefix, suffix := %s.%sPieces()", np.ParentPkg, name)
		// An aggregation declared AcrossDeleted counts the parent's marked
		// rows; every other read of the parent leaves them out.
		if np.ParentLive != "" && !agg.AcrossDeleted {
			g.p("		st = %s.Put(toks, runtime.SpliceTreeWhere(prefix, %s, toks, %sLowering, suffix))",
				cache, lit(np.ParentLive), lowerFirst(q))
		} else {
			g.p("		st = %s.Put(toks, runtime.SpliceTree(prefix, toks, %sLowering, suffix))", cache, lowerFirst(q))
		}
		g.p("	}")
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
		g.p("	rows, err := %s", g.dec.rowsFrom("ex.Query(ctx, st.SQL, args)"))
		g.p("	if err != nil {")
		g.p("		return nil, err")
		g.p("	}")
		g.p("	defer rows.Close()")
		g.p("	var sl runtime.Slab")
		g.p("	var out []%s.%sRow", np.ParentPkg, name)
		g.p("	for rows.Next() {")
		g.p("		out = append(out, %s.%sRow{})", np.ParentPkg, name)
		g.p("		if err := %s.Scan%s(rows.%s(), &out[len(out)-1], &sl); err != nil {", np.ParentPkg, name, g.dec.rowsAccessor())
		g.p("			return nil, err")
		g.p("		}")
		g.p("	}")
		g.p("	return out, rows.Err()")
		g.p("}")
		g.p("")
	}
}

// emitProbeInsertSelect writes the same INSERT … SELECT over a semi-join of
// the source, as a method of its composer.
func (g *gen) emitProbeInsertSelect(np havingSpec, lw lowering, s *schema.Schema, t *schema.Table, rel *schema.Relation) error {
	fn, tpkg, spkg, ref, splice, err := insertSelectFor(lw, s, t, rel)
	if err != nil {
		return err
	}
	q := np.TypeName
	cache := lowerFirst(q) + fn + "Cache"
	g.p("var %s = runtime.NewTreeCache()", cache)
	g.p("")
	g.p("// %s is the package function of that name over this", fn)
	g.p("// semi-join: one %s row per row it selects, in ONE statement.", t.Name)
	g.p("func (h %s) %s(ctx context.Context, ex %s, ins %s.Ins) (int64, error) {", q, fn, g.execType(), tpkg)
	g.p("	if err := h.err(); err != nil {")
	g.p("		return 0, err")
	g.p("	}")
	g.p("	if h.q.Locked() || h.q.Paged() {")
	g.p("		return 0, runtime.ErrPagedWrite")
	g.p("	}")
	g.p("	var buf [%d]runtime.Tok", g.composedBuf())
	g.p("	toks := h.tokens(buf[:0], false)")
	g.p("	head, args := ins.SelectHead(%d)", ref)
	g.p("	if head.Err != nil {")
	g.p("		return 0, head.Err")
	g.p("	}")
	g.p("	mask := ins.Assigned() &^ (1 << %d)", ref)
	g.p("	var key [%d]runtime.Tok", g.composedBuf()+2)
	g.p("	key[0], key[1] = runtime.Tok(mask), runtime.Tok(mask>>32)")
	g.p("	n := 2 + copy(key[2:], toks)")
	g.p("	st := %s.Get(key[:n])", cache)
	g.p("	if st == nil {")
	g.p("		_, _, limitSfx := %s.StmtPieces()", spkg)
	g.p("		st = %s.Put(key[:n], %s)", cache, splice(lowerFirst(q)+"Lowering"))
	g.p("	}")
	g.p("	if st.Err != nil {")
	g.p("		return 0, st.Err")
	g.p("	}")
	g.p("	pb := %s.GetBinder()", np.ParentPkg)
	g.p("	defer %s.PutBinder(pb)", np.ParentPkg)
	g.p("	bound, cbs := h.bind(pb, true)")
	g.p("	defer func() {")
	g.p("		for _, cb := range cbs {")
	g.p("			%s.PutBinder(cb)", np.ChildPkg)
	g.p("		}")
	g.p("	}()")
	g.p("	return ex.Exec(ctx, st.SQL, append(args, bound...))")
	g.p("}")
	g.p("")
	return nil
}
