package codegenmongo

import (
	"bytes"
	"fmt"
)

// ctxGen emits the context package's plans.
type ctxGen struct {
	b bytes.Buffer
}

func (c *ctxGen) p(format string, a ...any) {
	fmt.Fprintf(&c.b, format, a...)
	c.b.WriteByte('\n')
}

func (c *ctxGen) plan(p plan) {
	row, q, pp := p.name+"Row", p.name+"Query", p.parentPkg
	c.rowTypes(p.members)
	c.p("// %s is %s with %s loaded.", row, p.parent.Name, fieldList(p.members))
	c.p("type %s struct {", row)
	c.p("\t%s.Row", pp)
	for _, m := range p.members {
		c.p("\t%s", fieldDecl(m))
	}
	c.p("}")
	c.p("")
	c.p("// %s builds the plan over a %s query. Every builder method is", q, pp)
	c.p("// redeclared, since an embedded Query would return itself from Where.")
	c.p("type %s struct {", q)
	c.p("\tq          %s.Query", pp)
	c.p("\tchildLimit int64")
	c.p("}")
	c.p("")
	c.p("// %s starts the plan. It costs %d round trips: the parents, and one per", p.name, 1+count(p.members))
	c.p("// relation loaded.")
	c.p("func %s() %s { return %s{q: %s.New(), childLimit: defaultChildLimit} }", p.name, q, q, pp)
	c.p("")
	for _, m := range []struct{ name, params, call string }{
		{"Where", "ps ..." + pp + ".Pred", "Where(ps...)"},
		{"WhereIf", "cond bool, pr " + pp + ".Pred", "WhereIf(cond, pr)"},
		{"Any", "ps ..." + pp + ".Pred", "Any(ps...)"},
		{"Order", "ts ..." + pp + ".Sort", "Order(ts...)"},
		{"Limit", "n int64", "Limit(n)"},
		{"Offset", "n int64", "Offset(n)"},
	} {
		c.p("func (p %s) %s(%s) %s { p.q = p.q.%s; return p }", q, m.name, m.params, q, m.call)
		c.p("")
	}
	c.p("// Err reports what the parent query refused while it was built.")
	c.p("func (p %s) Err() error { return p.q.Err() }", q)
	c.p("")
	c.p("// ChildLimit caps each relation's load. A guard, not a page size.")
	c.p("func (p %s) ChildLimit(n int64) %s { p.childLimit = n; return p }", q, q)
	c.p("")
	c.p("// All runs the plan.")
	c.p("func (p %s) All(ctx context.Context, ex runtime.Executor) ([]%s, error) {", q, row)
	c.p("\tparents, err := p.q.List(ctx, ex)")
	c.p("\tif err != nil || len(parents) == 0 {")
	c.p("\t\treturn nil, err")
	c.p("\t}")
	c.p("\tout := make([]%s, len(parents))", row)
	c.p("\tfor i, r := range parents {")
	c.p("\t\tout[i].Row = r")
	c.p("\t}")
	for _, m := range p.members {
		c.p("\tif err := %s(ctx, ex, out, p.childLimit); err != nil {", m.loader)
		c.p("\t\treturn nil, err")
		c.p("\t}")
	}
	c.p("\treturn out, nil")
	c.p("}")
	c.p("")
	c.loaders(row, p.members)
}

func count(ms []member) int {
	n := len(ms)
	for _, m := range ms {
		n += count(m.nested)
	}
	return n
}

func fieldList(ms []member) string {
	s := ""
	for i, m := range ms {
		if i > 0 {
			s += ", "
		}
		s += m.rel.Field
	}
	return s
}

// fieldDecl is a loaded relation's field: a slice for to-many, a pointer for
// to-one — nil when the key is NULL or names nothing, which MongoDB, with no
// foreign keys, allows.
func fieldDecl(m member) string {
	if m.rel.ToMany {
		return m.rel.Field + " []" + m.elem
	}
	return m.rel.Field + " *" + m.elem
}

// rowTypes declares the row types nested members need, innermost first.
func (c *ctxGen) rowTypes(ms []member) {
	for _, m := range ms {
		if len(m.nested) == 0 {
			continue
		}
		c.rowTypes(m.nested)
		c.p("// %s is %s with %s loaded.", m.elem, m.child.Name, fieldList(m.nested))
		c.p("type %s struct {", m.elem)
		c.p("\t%s.Row", m.childPkg)
		for _, n := range m.nested {
			c.p("\t%s", fieldDecl(n))
		}
		c.p("}")
		c.p("")
	}
}

// loaders writes each member's loader, over a slice of holder rows.
func (c *ctxGen) loaders(holder string, ms []member) {
	for _, m := range ms {
		c.loader(holder, m)
		if len(m.nested) > 0 {
			c.loaders(m.elem, m.nested)
		}
	}
}

func (c *ctxGen) loader(holder string, m member) {
	c.p("// %s loads %s for every row in out with one $in.", m.loader, m.rel.Field)
	c.p("func %s(ctx context.Context, ex runtime.Executor, out []%s, limit int64) error {", m.loader, holder)
	c.p("\tat := make(map[%s][]int, len(out))", m.key)
	c.p("\tkeys := make([]%s, 0, len(out))", m.key)
	c.p("\tfor i := range out {")
	c.p("\t\t%s", read("k", "out[i]", m.on, m.nullOn, "continue"))
	c.p("\t\tif _, ok := at[k]; !ok {")
	c.p("\t\t\tkeys = append(keys, k)")
	c.p("\t\t}")
	c.p("\t\tat[k] = append(at[k], i)")
	c.p("\t}")
	c.p("\tif len(keys) == 0 {")
	c.p("\t\treturn nil")
	c.p("\t}")
	if m.rel.Owner {
		// At most one target per key: the limit is the key count.
		c.p("\trows, err := %s.New().Where(%s.%s.In(keys...)).Limit(int64(len(keys))).List(ctx, ex)", m.childPkg, m.childPkg, m.by)
	} else {
		c.p("\trows, err := %s.New().Where(%s.%s.In(keys...)).Limit(limit).List(ctx, ex)", m.childPkg, m.childPkg, m.by)
	}
	c.p("\tif err != nil {")
	c.p("\t\treturn err")
	c.p("\t}")
	if !m.rel.Owner {
		c.p("\tif int64(len(rows)) >= limit {")
		c.p("\t\t// A partial load is worse than a failed one: every count made")
		c.p("\t\t// from it is wrong and nothing says so.")
		c.p("\t\treturn runtime.ErrChildLimit")
		c.p("\t}")
	}
	if len(m.nested) > 0 {
		c.p("\tkids := make([]%s, len(rows))", m.elem)
		c.p("\tfor j, r := range rows {")
		c.p("\t\tkids[j].Row = r")
		c.p("\t}")
		for _, n := range m.nested {
			c.p("\tif err := %s(ctx, ex, kids, limit); err != nil {", n.loader)
			c.p("\t\treturn err")
			c.p("\t}")
		}
	} else {
		c.p("\tkids := rows")
	}
	c.p("\tfor j := range kids {")
	c.p("\t\t%s", read("k", "kids[j]", m.by, m.nullBy, "continue"))
	c.p("\t\tfor _, i := range at[k] {")
	switch {
	case m.rel.ToMany:
		c.p("\t\t\tout[i].%s = append(out[i].%s, kids[j])", m.rel.Field, m.rel.Field)
	default:
		c.p("\t\t\tif out[i].%s == nil {", m.rel.Field)
		c.p("\t\t\t\tv := kids[j]")
		c.p("\t\t\t\tout[i].%s = &v", m.rel.Field)
		c.p("\t\t\t}")
	}
	c.p("\t\t}")
	c.p("\t}")
	c.p("\treturn nil")
	c.p("}")
	c.p("")
}

// read is a statement binding name to row's field, leaving with skip when a
// nullable field is NULL.
func read(name, row, field string, nullable bool, skip string) string {
	if !nullable {
		return name + " := " + row + "." + field
	}
	return "if !" + row + "." + field + ".Valid {\n\t\t\t" + skip + "\n\t\t}\n\t\t" + name + " := " + row + "." + field + ".V"
}
