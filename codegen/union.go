package codegen

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// Declared UNION emission.
//
// Into the CONTEXT package, not a table package, because a union belongs to no
// table — that is the whole difficulty (ADR-0008). A join's row lives with its
// declaring table because there is one; a feed of comments, follows and
// releases has no such owner, so the row lives with the reads that span tables.
//
// The statement is entirely fixed. There is no call-site WHERE to splice, so
// there is no token stream, no tree cache and no binder here: one constant
// string, one placeholder for the limit — and the statement text, LIMIT and
// all, comes from compile/pgsql, because every dialect word storm emits lives
// in its back end (R9). That makes this the simplest read storm generates,
// which is a fair trade for the declaration being the strictest.
func (g *gen) emitUnion(u *schema.Union) {
	cols, err := unionCols(u)
	if err != nil {
		g.err = err
		return
	}
	low := lowerFirst(u.Name)

	g.p("// %sRow is the %q union: %d column(s) merged from %d table(s).",
		u.Name, u.Name, len(cols), len(u.Branches))
	g.p("//")
	g.p("// A column is nullable here when ANY branch can produce NULL for it,")
	g.p("// whatever the other branches' constraints say — typing it otherwise")
	g.p("// would decode one branch's NULL as another branch's zero value.")
	g.p("type %sRow struct {", u.Name)
	for _, c := range cols {
		g.p("\t%s %s", c.field, goType(c.col))
	}
	g.p("}")
	g.p("")

	g.p("const %sSQL = `%s%s`", low, pgsql.UnionSelect(u), pgsql.UnionSuffix(u))
	g.p("")

	fallible := false
	for _, c := range cols {
		if fallibleIn(c.col, g.dec) {
			fallible = true
		}
	}
	g.p("func scan%s(rv [][]byte, r *%sRow, sl *runtime.Slab) error {", u.Name, u.Name)
	if fallible {
		g.p("\tvar decErr error")
	}
	for i, c := range cols {
		g.p("\t%s", decodeExprIn(c.col, i, g.dec))
		if fallibleIn(c.col, g.dec) {
			g.p("\tif decErr != nil {")
			g.p("\t\treturn decErr")
			g.p("\t}")
		}
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")

	params, args := unionParams(u)

	g.p("// %s runs the %q union: every branch, merged, ordered as one, in ONE", u.Name, u.Name)
	g.p("// round trip. n caps the merged result, not any single branch — which is")
	g.p("// the difference between a feed and several lists the caller has to")
	g.p("// interleave itself.")
	if len(u.Params) > 0 {
		g.p("//")
		g.p("// A declared parameter used in several branches is ONE argument: the")
		g.p("// same value reaches every branch that names it, which is what")
		g.p("// \"this actor's feed\" has to mean.")
	}
	g.p("func %s(ctx context.Context, ex runtime.Executor%s, n int64) ([]%sRow, error) {", u.Name, params, u.Name)
	g.p("\tvar sl runtime.Slab")
	g.p("\treturn %sInto(ctx, ex, nil, &sl%s, n)", u.Name, args)
	g.p("}")
	g.p("")
	g.p("// %sInto lets the caller own the output slice and the string arena.", u.Name)
	g.p("func %sInto(ctx context.Context, ex runtime.Executor, dst []%sRow, sl *runtime.Slab%s, n int64) ([]%sRow, error) {",
		u.Name, u.Name, params, u.Name)
	g.p("\trows, err := ex.Query(ctx, %sSQL, []any{%sn})", low, args2(u))
	g.p("\tif err != nil {")
	g.p("\t\treturn dst, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tfor rows.Next() {")
	g.p("\t\tdst = append(dst, %sRow{})", u.Name)
	g.p("\t\tif err := scan%s(rows.RawValues(), &dst[len(dst)-1], sl); err != nil {", u.Name)
	g.p("\t\t\treturn dst, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn dst, rows.Err()")
	g.p("}")
	g.p("")
}

// unionCols resolves the row type from the shared output shape.
func unionCols(u *schema.Union) ([]aggCol, error) {
	out := make([]aggCol, 0, len(u.Cols))
	seen := map[string]bool{}
	for _, c := range u.Cols {
		if seen[c.As] {
			return nil, fmt.Errorf("codegen: union %s: two outputs named %s", u.Name, c.As)
		}
		seen[c.As] = true
		sc := &schema.Column{
			Name:    pgsql.ColumnCase(c.As),
			Type:    c.Type,
			NotNull: !c.Nullable,
		}
		if goKind(sc) == kindUnsupported {
			return nil, fmt.Errorf(
				"codegen: union %s: %s is %s, which has no Go type yet",
				u.Name, c.As, c.Type.SQL())
		}
		out = append(out, aggCol{col: sc, field: c.As})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("codegen: union %s selects nothing", u.Name)
	}
	return out, nil
}

// unionsNeedTime reports whether any union row carries a time column, which is
// the one import the context package cannot infer from the reads it already
// emits — a union's row is scalars and belongs to no table package.
func unionsNeedTime(us []*schema.Union) bool {
	for _, u := range us {
		cols, err := unionCols(u)
		if err != nil {
			continue // the real error is reported when the union is emitted
		}
		for _, c := range cols {
			if strings.Contains(goType(c.col), "time.") {
				return true
			}
		}
	}
	return false
}

// unionParams renders the declared parameters as a Go signature fragment and
// the matching argument list.
//
// Declaration order is signature order is placeholder order. One rule for all
// three, so a reader of the generated function does not have to hold a mapping
// in their head.
func unionParams(u *schema.Union) (sig, args string) {
	for _, p := range u.Params {
		name := lowerFirst(p.Name)
		sig += ", " + name + " " + goType(&schema.Column{
			Name: p.Name, Type: p.Type, NotNull: true,
		})
		args += ", " + name
	}
	return sig, args
}

// args2 is the same list as a leading argument sequence for the []any.
func args2(u *schema.Union) string {
	var s string
	for _, p := range u.Params {
		s += lowerFirst(p.Name) + ", "
	}
	return s
}
