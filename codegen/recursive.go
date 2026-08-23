package codegen

import (
	"fmt"

	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Recursive traversal emission for a self-referencing table.
//
// Generated only when the model actually declares a self-reference — a table
// with no parent column gets no Descend, so calling it is a compile error
// rather than a runtime one.

// selfRefColumn is the column by which this table references itself, or "".
//
// It reads the declared relations rather than the foreign keys, because a
// foreign key back to the same table can be an ordinary link (a "created_by"
// pointing at users from users) and only the model knows which one is the
// hierarchy. A table with more than one self-reference is ambiguous and is left
// alone rather than guessed at.
func selfRefColumn(t *schema.Table) (string, error) {
	var found []string
	for _, rel := range t.Relations {
		if rel.Target != t.Name || rel.ToMany || rel.Column == "" {
			continue
		}
		found = append(found, rel.Column)
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf(
			"codegen: table %s references itself through %v — recursive traversal cannot "+
				"tell which is the hierarchy; keep one self-reference per table",
			t.Name, found)
	}
}

func (g *gen) recursive() {
	parent, err := selfRefColumn(g.t)
	if err != nil {
		g.err = err
		return
	}
	if parent == "" {
		return
	}
	if len(g.t.PrimaryKey) != 1 {
		return // a composite key has no single array to guard a cycle with
	}
	key := g.t.PrimaryKey[0]
	kc := g.t.Column(key)
	if kc == nil {
		return
	}
	cols := readableCols(g.t)
	keyGo := baseGoType(kc)

	g.p("// Recursive traversal of %s's self-reference through %s.", g.t.Name, parent)
	g.p("//")
	g.p("// ONE query for a whole subtree, with a MANDATORY depth bound and a cycle")
	g.p("// guard. Neither is optional and neither has a default:")
	g.p("//")
	g.p("// An unbounded recursive query against production data is an outage, not")
	g.p("// a slow query — it is the one shape where a missing bound turns into")
	g.p("// unbounded work rather than a large result.")
	g.p("//")
	g.p("// And %s is a foreign key, which does not stop A pointing at B pointing", parent)
	g.p("// at A. The guard is an explicit path array: one array append per row,")
	g.p("// against a hung connection.")
	g.p("// ErrDepth is returned by a traversal given no positive depth bound.")
	g.p("var ErrDepth = errors.New(")
	g.p("\t%q)", "raorm: recursive traversal needs a positive depth bound — "+
		"unbounded recursion over a cycle does not return")
	g.p("")

	for _, dir := range []struct {
		name, doc string
		dir       int
	}{
		{"Descend", "descendants: rows whose " + parent + " chain leads back to a root", pgsql.Descend},
		{"Ascend", "ancestors: the " + parent + " chain upward from each row", pgsql.Ascend},
	} {
		sql := pgsql.Recursive(g.t.Name, cols, key, parent, dir.dir)
		g.p("// %s returns the %s.", dir.name, dir.doc)
		g.p("//")
		g.p("// The roots themselves are included, at depth 1. maxDepth counts them,")
		g.p("// so maxDepth of 1 returns exactly the roots and 2 adds one level.")
		g.p("//")
		g.p("// Rows come back in no guaranteed order — a tree has no total order")
		g.p("// and inventing one would be a lie. Every row carries its %s, so the", parent)
		g.p("// caller reassembles the shape it wanted.")
		g.p("func %s(ctx context.Context, ex runtime.Executor, roots []%s, maxDepth int64) ([]Row, error) {", dir.name, keyGo)
		g.p("\tif len(roots) == 0 {")
		g.p("\t\treturn nil, nil")
		g.p("\t}")
		g.p("\tif maxDepth <= 0 {")
		g.p("\t\treturn nil, ErrDepth")
		g.p("\t}")
		g.p("\trows, err := ex.Query(ctx, %sSQL, []any{roots, maxDepth})", lowerFirst(dir.name))
		g.p("\tif err != nil {")
		g.p("\t\treturn nil, err")
		g.p("\t}")
		g.p("\tdefer rows.Close()")
		g.p("\tvar sl runtime.Slab")
		g.p("\tvar out []Row")
		g.p("\tfor rows.Next() {")
		g.p("\t\tout = append(out, Row{})")
		g.p("\t\tscan(rows.RawValues(), &out[len(out)-1], &sl)")
		g.p("\t}")
		g.p("\treturn out, rows.Err()")
		g.p("}")
		g.p("")
		g.p("const %sSQL = `%s`", lowerFirst(dir.name), sql)
		g.p("")
	}
}
