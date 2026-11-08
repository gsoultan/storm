// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegenmongo

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// member is one relation a plan loads, and what is loaded through it.
type member struct {
	rel           *schema.Relation
	parent, child *schema.Table
	childPkg      string
	// on is the holder's field the join value is read from, at the child's
	// column by; nullOn and nullBy say whether each is runtime.Null.
	on, by         string
	nullOn, nullBy bool
	key            string // the join value's Go type
	// elem is what the field holds: the child's Row, or a row type of
	// this plan's when something is loaded through it.
	elem   string
	loader string
	nested []member
	// embedded: the child is held in the parent's documents, and loaded by
	// its package's Load rather than a query.
	embedded bool
}

// plans enumerates a context's plans. A relation whose far table is not in
// this generation has no Row type to name, and is left out, as on SQL.
func plans(s *schema.Schema, names []string) ([]plan, error) {
	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}
	var out []plan
	for _, name := range names {
		t := s.Table(name)
		if mongo.EmbedOf(t) != nil {
			// No query to start a plan from: its rows are read through
			// the parent.
			continue
		}
		pkg, err := codegen.PackageName(t.GoName, t.Name)
		if err != nil {
			return nil, err
		}
		for _, r := range t.Relations {
			if !in[r.Target] || r.Link != "" {
				continue
			}
			p := plan{name: schema.GoName(t.GoName) + "With" + r.Field, parent: t, parentPkg: pkg}
			m, err := resolve(s, in, t, p.name, schema.PlanField{Field: r.Field})
			if err != nil {
				return nil, err
			}
			p.members = []member{m}
			out = append(out, p)
		}
		for _, pl := range t.Plans {
			p := plan{name: schema.GoName(t.GoName) + pl.Name, parent: t, parentPkg: pkg}
			for _, f := range pl.Fields {
				m, err := resolve(s, in, t, p.name, f)
				if err != nil {
					return nil, err
				}
				p.members = append(p.members, m)
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	for i := 1; i < len(out); i++ {
		if out[i].name == out[i-1].name {
			return nil, fmt.Errorf("codegenmongo: two plans are both named %s — rename the declared one", out[i].name)
		}
	}
	return out, nil
}

// resolve is one plan field, against the relation it names. prefix names the
// row types it needs.
func resolve(s *schema.Schema, in map[string]bool, t *schema.Table, prefix string, f schema.PlanField) (member, error) {
	var r *schema.Relation
	for _, x := range t.Relations {
		if x.Field == f.Field {
			r = x
		}
	}
	if r == nil {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s names %s, which is not a relation", t.Name, prefix, f.Field)
	}
	if !in[r.Target] {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s loads %s from %s, which is not generated with it",
			t.Name, prefix, f.Field, r.Target)
	}
	if r.Link != "" {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s loads %s through a join table, which this back end does not generate",
			t.Name, prefix, f.Field)
	}
	child := s.Table(r.Target)
	cpkg, err := codegen.PackageName(child.GoName, child.Name)
	if err != nil {
		return member{}, err
	}
	m := member{rel: r, parent: t, child: child, childPkg: cpkg, elem: cpkg + ".Row",
		loader: "load" + prefix + f.Field, embedded: mongo.EmbedOf(child) != nil}
	// The join: an owning side holds the key and the child is found by what
	// it references; an inverse side is found by the child's column, against
	// what that column references here.
	var on, by *schema.Column
	if r.Owner {
		on = t.Column(r.Column)
		by = child.Column(referenced(t, r.Column, child))
	} else {
		by = child.Column(r.Column)
		on = t.Column(referenced(child, r.Column, t))
	}
	if on == nil || by == nil {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s loads %s over a composite key, which this back end does not generate",
			t.Name, prefix, f.Field)
	}
	ko, ok1 := kindOf(on)
	kb, ok2 := kindOf(by)
	if !ok1 || !ok2 || ko.goType != kb.goType || !kb.listable {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s joins %s on %s and %s, which cannot be matched with $in",
			t.Name, prefix, f.Field, on.Name, by.Name)
	}
	m.on, m.by = schema.GoName(on.Name), schema.GoName(by.Name)
	m.nullOn, m.nullBy = ko.nullable(on), kb.nullable(by)
	m.key = ko.goType
	if len(f.Nested) > 0 && m.embedded {
		return member{}, fmt.Errorf("codegenmongo: %s: plan %s loads through %s, which is embedded and refers to nothing but its parent",
			t.Name, prefix, f.Field)
	}
	if len(f.Nested) > 0 {
		m.elem = prefix + f.Field + "Row"
		for _, n := range f.Nested {
			nm, err := resolve(s, in, child, prefix+f.Field, n)
			if err != nil {
				return member{}, err
			}
			m.nested = append(m.nested, nm)
		}
	}
	return m, nil
}

// Context renders the context package's file: every plan, as one Go file
// named pkg, importing the table packages from imp.
func Context(s *schema.Schema, names []string, pkg, imp, storm string) ([]byte, error) {
	ps, err := plans(s, names)
	if err != nil {
		return nil, err
	}
	var c ctxGen
	pkgs := map[string]bool{}
	usesTime := false
	var walk func([]member)
	walk = func(ms []member) {
		for _, m := range ms {
			pkgs[m.childPkg] = true
			usesTime = usesTime || strings.HasPrefix(m.key, "time.")
			walk(m.nested)
		}
	}
	for _, p := range ps {
		pkgs[p.parentPkg] = true
		walk(p.members)
		c.plan(p)
	}
	for _, u := range s.Unions {
		ut, err := c.union(s, u)
		if err != nil {
			return nil, err
		}
		usesTime = usesTime || ut
	}
	var h bytes.Buffer
	fmt.Fprintf(&h, "// Code generated by storm for MongoDB. DO NOT EDIT.\n\n")
	fmt.Fprintf(&h, "// Package %s loads relations: one type per relation and per declared plan,\n", pkg)
	fmt.Fprintf(&h, "// each costing one round trip for the parents and one per relation loaded,\n")
	fmt.Fprintf(&h, "// whatever the row count — the children are found with one $in.\n")
	fmt.Fprintf(&h, "package %s\n\nimport (\n\t\"context\"\n", pkg)
	if len(s.Unions) > 0 {
		fmt.Fprintf(&h, "\t\"errors\"\n")
	}
	if usesTime {
		fmt.Fprintf(&h, "\t\"time\"\n")
	}
	fmt.Fprintf(&h, "\n")
	var sorted []string
	for p := range pkgs {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		fmt.Fprintf(&h, "\t%q\n", imp+"/"+p)
	}
	fmt.Fprintf(&h, "\t%q\n", storm+"/runtime")
	if len(s.Unions) > 0 {
		fmt.Fprintf(&h, "\t%q\n", storm+"/runtime/bsondec")
	}
	fmt.Fprintf(&h, ")\n\n")
	if usesTime {
		fmt.Fprintf(&h, "var _ = time.Time{}\n\n")
	}
	fmt.Fprintf(&h, "// defaultChildLimit caps one relation's load. Reaching it is an error, never\n")
	fmt.Fprintf(&h, "// a partial result; ChildLimit overrides it per plan.\n")
	fmt.Fprintf(&h, "const defaultChildLimit = 1 << 20\n\n")
	h.Write((*bytes.Buffer)(&c).Bytes())
	src, err := format.Source(h.Bytes())
	if err != nil {
		return nil, fmt.Errorf("codegenmongo: the context file does not parse (a generator bug): %w\n%s", err, h.Bytes())
	}
	return src, nil
}

// referenced is the column of to that from's column col references: the
// foreign key's, or to's single-column primary key, or "".
func referenced(from *schema.Table, col string, to *schema.Table) string {
	for _, fk := range from.ForeignKeys {
		if len(fk.Columns) == 1 && fk.Columns[0] == col && fk.RefTable == to.Name && len(fk.RefColumns) == 1 {
			return fk.RefColumns[0]
		}
	}
	if len(to.PrimaryKey) == 1 {
		return to.PrimaryKey[0]
	}
	return ""
}

// ctxGen emits the context package's plans.
type ctxGen bytes.Buffer

func (c *ctxGen) p(format string, a ...any) {
	fmt.Fprintf((*bytes.Buffer)(c), format, a...)
	(*bytes.Buffer)(c).WriteByte('\n')
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
	if m.embedded {
		c.embedLoader(holder, m)
		return
	}
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

// embedLoader reads embedded children through their package's Load: one
// command for every parent's array.
func (c *ctxGen) embedLoader(holder string, m member) {
	c.p("// %s loads %s, held in each parent's document, with one $in.", m.loader, m.rel.Field)
	c.p("func %s(ctx context.Context, ex runtime.Executor, out []%s, limit int64) error {", m.loader, holder)
	c.p("\tkeys := make([]%s, len(out))", m.key)
	c.p("\tfor i := range out {")
	c.p("\t\tkeys[i] = out[i].%s", m.on)
	c.p("\t}")
	c.p("\tby, err := %s.Load(ctx, ex, keys...)", m.childPkg)
	c.p("\tif err != nil {")
	c.p("\t\treturn err")
	c.p("\t}")
	c.p("\tn := int64(0)")
	c.p("\tfor i := range out {")
	c.p("\t\trs := by[out[i].%s]", m.on)
	c.p("\t\tn += int64(len(rs))")
	if m.rel.ToMany {
		c.p("\t\tout[i].%s = rs", m.rel.Field)
	} else {
		c.p("\t\tif len(rs) > 0 {")
		c.p("\t\t\tout[i].%s = &rs[0]", m.rel.Field)
		c.p("\t\t}")
	}
	c.p("\t}")
	c.p("\tif n >= limit {")
	c.p("\t\treturn runtime.ErrChildLimit")
	c.p("\t}")
	c.p("\treturn nil")
	c.p("}")
	c.p("")
}

// union emits a declared union: its row, its one command, and the function
// that runs it — parameters first, then the cap on the merged rows. It
// reports whether the row or a parameter is a time.
func (c *ctxGen) union(s *schema.Schema, u *schema.Union) (bool, error) {
	cmd, err := mongo.Union(s, u)
	if err != nil {
		return false, fmt.Errorf("codegenmongo: union %s: %w", u.Name, err)
	}
	usesTime := false
	row := u.Name + "Row"
	g := &gen{}
	c.p("// %s is the %q union: %d column(s) merged from %d collection(s), a", row, u.Name, len(u.Cols), len(u.Branches))
	c.p("// column nullable when any branch can leave it NULL.")
	c.p("type %s struct {", row)
	for i, col := range u.Cols {
		res := &schema.Column{Type: col.Type, NotNull: !col.Nullable}
		k, ok := kindOf(res)
		if !ok {
			return false, fmt.Errorf("codegenmongo: union %s: %s has no MongoDB form", u.Name, col.As)
		}
		usesTime = usesTime || k.goType == "time.Time"
		c.p("\t%s %s", col.As, k.fieldType(res))
		g.decodeField(i, col.As, k, k.nullable(res), "union "+u.Name+"."+col.As)
	}
	c.p("}")
	c.p("")
	lower := strings.ToLower(u.Name[:1]) + u.Name[1:]
	c.p("const %sCommand = %s", lower, strconv.Quote(cmd))
	c.p("")
	c.p("func scan%s(rv [][]byte, r *%s) error {", u.Name, row)
	c.p("\tif len(rv) != %d {", len(u.Cols))
	c.p("\t\treturn runtime.ErrShape")
	c.p("\t}")
	c.p("\tvar err error")
	(*bytes.Buffer)(c).Write(g.b.Bytes())
	c.p("\treturn nil")
	c.p("}")
	c.p("")
	var params, args []string
	for _, p := range u.Params {
		k, ok := kindOf(&schema.Column{Type: p.Type, NotNull: true})
		if !ok || k.slice {
			return false, fmt.Errorf("codegenmongo: union %s: parameter %s has no MongoDB form", u.Name, p.Name)
		}
		usesTime = usesTime || k.goType == "time.Time"
		name := strings.ToLower(p.Name[:1]) + p.Name[1:]
		params = append(params, name+" "+k.goType)
		args = append(args, name)
	}
	args = append(args, "n")
	c.p("// %s runs the %q union: every branch, merged, ordered as one, in one", u.Name, u.Name)
	c.p("// round trip. n caps the merged rows, not any one branch.")
	c.p("func %s(ctx context.Context, ex runtime.Executor%s, n int64) ([]%s, error) {", u.Name, prefixed(params), row)
	c.p("\tif n <= 0 {")
	c.p("\t\treturn nil, nil")
	c.p("\t}")
	c.p("\trows, err := ex.Query(ctx, %sCommand, []any{%s})", lower, strings.Join(args, ", "))
	c.p("\tif err != nil {")
	c.p("\t\treturn nil, err")
	c.p("\t}")
	c.p("\tdefer rows.Close()")
	c.p("\tvar out []%s", row)
	c.p("\tfor rows.Next() {")
	c.p("\t\tvar r %s", row)
	c.p("\t\tif err := scan%s(rows.RawValues(), &r); err != nil {", u.Name)
	c.p("\t\t\treturn out, err")
	c.p("\t\t}")
	c.p("\t\tout = append(out, r)")
	c.p("\t}")
	c.p("\treturn out, rows.Err()")
	c.p("}")
	c.p("")
	return usesTime, nil
}

func prefixed(params []string) string {
	if len(params) == 0 {
		return ""
	}
	return ", " + strings.Join(params, ", ")
}
