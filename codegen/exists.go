package codegen

import (
	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Relation-existence predicates: the first native join shape.
//
// `user.HasPosts()` is a semi-join — EXISTS over the relation's foreign key —
// and it is the join shape access-control code lives on ("users who have a
// published post" is one child-filter away; "users who have any post" is
// this). It fits the compilation model exactly because the fragment is
// CONSTANT: no bound value, so the predicate is one more (pseudo-column,
// operator) pair in the frag table, and And/Or/Not composition falls out of
// the token stream with no new machinery.
//
// The pseudo-columns extend the frag table PAST the real columns. Everything
// else indexed by column — the order table, the ident table, the arenas — is
// bounded by nCols and never sees them, which the bounds checks already
// enforce: a pseudo-column cannot be ordered by, cursored on, or bound.

// existsRelations is every relation of this table whose existence is testable:
// the ones whose key lives on the OTHER side (has-many, and the inverse end of
// a one-to-one). An owned to-one needs no predicate — its existence is the
// foreign-key column's IsNotNull, which already generates.
func existsRelations(t *schema.Table) []*schema.Relation {
	if len(t.PrimaryKey) != 1 {
		return nil // the correlation needs a single parent key
	}
	var out []*schema.Relation
	for _, rel := range t.Relations {
		if !rel.Owner && rel.Column != "" {
			out = append(out, rel)
		}
	}
	return out
}

// existsPreds emits Has<Field> and HasNo<Field> for each testable relation.
func (g *gen) existsPreds() {
	rels := existsRelations(g.t)
	if len(rels) == 0 {
		return
	}
	g.p("// Relation existence — the semi-join. HasPosts() matches rows with at")
	g.p("// least one related row; HasNoPosts() matches the rest. Both compose")
	g.p("// under Where/Any/Not like any predicate, because both lower to constant")
	g.p("// fragments: no bound value, one compiled statement per structure.")
	for i, rel := range rels {
		name := exportName(rel.Field)
		g.p("func Has%s() Pred    { return Pred{col: %d, op: opExists} }", name, len(g.cols)+i)
		g.p("func HasNo%s() Pred  { return Pred{col: %d, op: opNotExists} }", name, len(g.cols)+i)
	}
	g.p("")
}

// existsFragRows appends one frag-table row per testable relation: empty for
// every value operator, filled only at the exists slots.
func (g *gen) existsFragRows() {
	rels := existsRelations(g.t)
	if len(rels) == 0 {
		return
	}
	pk := g.t.PrimaryKey[0]
	for _, rel := range rels {
		g.p("\t{ // relation %s (pseudo-column)", rel.Field)
		g.p("\t\t{}, // opNone")
		for range ops {
			g.p("\t\t{},")
		}
		g.p("\t\t{A: %q},", pgsql.ExistsFrag(rel.Target, rel.Column, g.t.Name, pk))
		g.p("\t\t{A: %q},", pgsql.NotExistsFrag(rel.Target, rel.Column, g.t.Name, pk))
		g.p("\t},")
	}
}
