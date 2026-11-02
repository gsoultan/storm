package codegenmongo

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strings"

	"github.com/gsoultan/storm/codegen"
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
		loader: "load" + prefix + f.Field}
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
	var h bytes.Buffer
	fmt.Fprintf(&h, "// Code generated by storm for MongoDB. DO NOT EDIT.\n\n")
	fmt.Fprintf(&h, "// Package %s loads relations: one type per relation and per declared plan,\n", pkg)
	fmt.Fprintf(&h, "// each costing one round trip for the parents and one per relation loaded,\n")
	fmt.Fprintf(&h, "// whatever the row count — the children are found with one $in.\n")
	fmt.Fprintf(&h, "package %s\n\nimport (\n\t\"context\"\n", pkg)
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
	fmt.Fprintf(&h, "\t%q\n)\n\n", storm+"/runtime")
	if usesTime {
		fmt.Fprintf(&h, "var _ = time.Time{}\n\n")
	}
	fmt.Fprintf(&h, "// defaultChildLimit caps one relation's load. Reaching it is an error, never\n")
	fmt.Fprintf(&h, "// a partial result; ChildLimit overrides it per plan.\n")
	fmt.Fprintf(&h, "const defaultChildLimit = 1 << 20\n\n")
	h.Write(c.b.Bytes())
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
