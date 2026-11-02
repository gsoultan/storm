package mgddl

import (
	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// checkEmbed holds an embedded table (storm.Embed) to what an array inside
// another document can be.
//
// An element has no life of its own: it goes when its parent goes, it cannot
// be pointed at, and a constraint across elements is one MongoDB cannot
// state — a unique index constrains values across documents, not within one
// array. So the reference must be NOT NULL and ON DELETE CASCADE, nothing may
// reference the child, and what would need a collection of its own — soft
// delete, versions, guarded counters, uniqueness, CHECK, plans — is refused.
func checkEmbed(s *schema.Schema, t *schema.Table, r *schema.Relation, add func(pos, format string, a ...any)) {
	n := t.Name
	c := t.Column(r.Column)
	pos := colPos(t, c)
	embeds := 0
	for _, x := range t.Relations {
		switch {
		case x.Owner && x.Document == mongo.Embed:
			embeds++
		case x.Owner:
			add(pos, "%s: an embedded table's only reference is to its parent; %s refers elsewhere", n, x.Field)
		}
	}
	if embeds > 1 {
		add(pos, "%s: embedded in two parents — an element lives in one array", n)
	}
	if p := s.Table(r.Target); p != nil && mongo.EmbedOf(p) != nil {
		add(pos, "%s: embedded in %s, which is itself embedded — one level of embedding is generated", n, p.Name)
	}
	if c != nil && !c.NotNull {
		add(pos, "%s.%s: an embedded row cannot be without its parent — the reference must be NOT NULL", n, c.Name)
	}
	for _, fk := range t.ForeignKeys {
		if len(fk.Columns) == 1 && fk.Columns[0] == r.Column && fk.OnDelete != schema.Cascade {
			add(pos, "%s.%s: an embedded row goes with its parent's document — declare OnDelete(storm.Cascade), "+
				"which is what happens", n, r.Column)
		}
	}
	for _, fk := range t.ForeignKeys {
		if p := s.Table(r.Target); p != nil && len(fk.Columns) == 1 && fk.Columns[0] == r.Column &&
			len(fk.RefColumns) == 1 && !sameCols(p.PrimaryKey, fk.RefColumns) {
			add(pos, "%s.%s: an embedded row is found by its parent's _id — the reference must be to %s's key",
				n, r.Column, p.Name)
		}
	}
	if len(t.PrimaryKey) != 1 {
		add(t.Pos, "%s: an embedded table needs a single-column key, which addresses its element", n)
	}
	for _, o := range s.Tables {
		for _, fk := range o.ForeignKeys {
			if fk.RefTable == n {
				add(o.Pos, "%s: references %s, which is embedded — an element cannot be pointed at", o.Name, n)
			}
		}
	}
	refuse := []struct {
		on   bool
		what string
	}{
		{t.SoftDeletes(), "soft delete"},
		{len(t.Uniques) > 0, "a UNIQUE constraint — MongoDB's unique index does not see within one array"},
		{len(t.Checks) > 0, "a CHECK constraint"},
		{len(t.Plans) > 0, "plans"},
		{len(t.Projections) > 0, "projections — it has no query of its own"},
		{len(t.Aggregates) > 0, "aggregations — it has no query of its own"},
		{len(t.Joins) > 0, "joins — it has no query of its own"},
	}
	for _, c := range t.Columns {
		refuse = append(refuse, struct {
			on   bool
			what string
		}{c.Version || c.IncWhen != "", "a version column or a guarded increment"})
	}
	for _, ix := range t.Indexes {
		refuse = append(refuse, struct {
			on   bool
			what string
		}{ix.Unique, "a unique index — MongoDB's unique index does not see within one array"})
	}
	seen := map[string]bool{}
	for _, f := range refuse {
		if f.on && !seen[f.what] {
			seen[f.what] = true
			add(t.Pos, "%s: an embedded table cannot have %s", n, f.what)
		}
	}
}

// elements is the parent's validator property for each table embedded in it:
// an array of objects, each the child's columns but the reference, which the
// parent's own _id already says.
func elements(s *schema.Schema, parent *schema.Table, enums map[string]*schema.Enum) []string {
	var out []string
	for _, t := range mongo.Embedded(s, parent) {
		r := mongo.EmbedOf(t)
		var props, required []string
		for _, c := range t.Columns {
			if c.Name == r.Column {
				continue
			}
			f := mongo.Field(t, c.Name)
			props = append(props, mongo.Str(f)+`:`+property(c, enums))
			if c.NotNull {
				required = append(required, mongo.Str(f))
			}
		}
		item := `{"bsonType":"object"`
		if len(required) > 0 {
			item += `,"required":[` + join(required) + `]`
		}
		item += `,"properties":{` + join(props) + `}}`
		out = append(out, mongo.Str(t.Name)+`:{"bsonType":"array","items":`+item+`}`)
	}
	return out
}

// embeddedIndexes are the parent's multikey indexes on its embedded tables'
// columns: an index on a child column is one on "<child>.<field>" here. The
// reference's own index has nothing to index — elements do not store it.
func embeddedIndexes(s *schema.Schema, parent *schema.Table) []string {
	var out []string
	for _, t := range mongo.Embedded(s, parent) {
		r := mongo.EmbedOf(t)
		for _, ix := range t.Indexes {
			if len(ix.Columns) == 1 && ix.Columns[0].Name == r.Column {
				continue
			}
			name := ix.Name
			if name == "" {
				name = t.IndexName(ix)
			}
			var key []string
			for _, k := range ix.Columns {
				dir := "1"
				if k.Desc {
					dir = "-1"
				}
				key = append(key, mongo.Str(t.Name+"."+mongo.Field(t, k.Name))+":"+dir)
			}
			out = append(out, `{"key":{`+join(key)+`},"name":`+mongo.Str(name)+`}`)
		}
	}
	return out
}

func join(parts []string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += ","
		}
		s += p
	}
	return s
}
