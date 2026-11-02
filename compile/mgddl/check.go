// Package mgddl renders storm's schema IR as MongoDB collection definitions —
// a $jsonSchema validator and the indexes — and refuses what does not port.
//
// MongoDB has no DDL in the SQL sense. What a relational schema enforces with
// column types and NOT NULL, a collection enforces with a validator; what it
// enforces with UNIQUE, an index does. Both are emitted as commands, and
// storm applies neither (ADR-0001): `storm ddl -dialect mongo` prints them for
// whoever runs migrations, the way every other target's DDL is printed.
//
// The refusals are the point of Check. A document store differs from a
// relational one in shape, not syntax (ADR-0004), and every construct below is
// either SQL text MongoDB cannot evaluate or a guarantee it cannot make. Each
// is a sentence at generate time rather than a surprise in production.
package mgddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// Check reports every construct in s that has no MongoDB form, or nil.
func Check(s *schema.Schema) error {
	var p []string
	add := func(pos, format string, a ...any) {
		p = append(p, "  "+at(pos)+fmt.Sprintf(format, a...))
	}
	for _, u := range s.Unions {
		add("", "union %s: a UNION reads several tables into one result, and MongoDB's "+
			"$unionWith is not generated yet — declare it on a SQL target", u.Name)
	}
	for _, f := range s.Functions {
		add("", "function %s: a SQL-bodied routine has no MongoDB form", f.Name)
	}
	for _, v := range s.Views {
		add("", "view %s: a SQL view has no MongoDB form", v.Name)
	}
	for _, tr := range s.Triggers {
		add("", "trigger %s: a SQL trigger has no MongoDB form", tr.Name)
	}
	for _, t := range s.Tables {
		checkTable(t, add)
	}
	if len(p) == 0 {
		return nil
	}
	sort.Strings(p)
	return fmt.Errorf("this model does not port to MongoDB:\n%s", strings.Join(p, "\n"))
}

func checkTable(t *schema.Table, add func(pos, format string, a ...any)) {
	n := t.Name
	if len(t.PrimaryKey) == 0 {
		add(t.Pos, "%s: needs a primary key — it is stored as _id", n)
	}
	if t.Generated {
		add(t.Pos, "%s: a generated many-to-many join table — a document store models "+
			"this as an array of references, which storm does not generate yet", n)
	}
	sqlOnly := []struct {
		on   bool
		what string
	}{
		{t.Sharded(), "a shard key"},
		{t.Partition != nil || t.PartitionOf != "", "partitioning"},
		{len(t.Checks) > 0, "CHECK constraints (SQL text)"},
		{len(t.Excludes) > 0, "EXCLUDE constraints"},
		{len(t.Arcs) > 0 || len(t.AnyRefs) > 0, "polymorphic references"},
		{len(t.Plans) > 0, "named plans (relation loading is not generated yet)"},
		{len(t.Projections) > 0, "named projections (not generated yet)"},
		{len(t.Aggregates) > 0, "declared aggregations (not generated yet)"},
		{len(t.Joins) > 0, "declared joins — MongoDB has no non-equality join, and $lookup is not generated yet"},
		{len(t.Matches) > 0 || len(t.Probes) > 0, "existence probes"},
		{t.Takes || t.Tallies || len(t.InsertSelects) > 0, "set-based write shapes (Take, Tally, InsertSelect)"},
		{t.Turns != "", "TakesTurns"},
		{t.Lockable, "table locks"},
		{t.GuardsUpsert, "guarded upserts"},
	}
	for _, f := range sqlOnly {
		if f.on {
			add(t.Pos, "%s: %s has no MongoDB form", n, f.what)
		}
	}
	for _, c := range t.Columns {
		checkColumn(t, c, add)
	}
	for _, fk := range t.ForeignKeys {
		checkReference(t, fk, add)
	}
	for _, ix := range t.Indexes {
		checkIndex(t, ix, add)
	}
	for _, u := range t.Uniques {
		for _, col := range u.Columns {
			if c := t.Column(col); c != nil && (c.Type.Array || mongo.IsJSON(c)) {
				add(colPos(t, c), "%s: UNIQUE over %s, an array or json column — MongoDB's unique "+
					"index on one constrains each ELEMENT or field, not the value", t.Name, col)
			}
		}
	}
}

func checkColumn(t *schema.Table, c *schema.Column, add func(pos, format string, a ...any)) {
	pos := colPos(t, c)
	if _, ok := mongo.BSONType(c); !ok {
		add(pos, "%s.%s: type %s has no MongoDB form storm generates", t.Name, c.Name, c.Type.SQL())
	}
	if c.Generated != "" {
		add(pos, "%s.%s: a generated column's expression is SQL text, which MongoDB cannot evaluate",
			t.Name, c.Name)
	}
	if c.Identity || c.Serial {
		add(pos, "%s.%s: MongoDB has no sequence; an auto-incrementing key is a counter "+
			"document and a race — use storm.Model's uuid key", t.Name, c.Name)
	}
	switch k, _ := mongo.ClassifyDefault(c); k {
	case mongo.DefaultUnsupported:
		add(pos, "%s.%s: default %s is SQL text MongoDB cannot evaluate — storm writes now(), "+
			"gen_random_uuid(), uuidv7() and literals", t.Name, c.Name, c.Default)
	case mongo.DefaultUUIDv4, mongo.DefaultUUIDv7:
		if c.Type.Name != schema.TypeUUID {
			add(pos, "%s.%s: a uuid default on a %s column", t.Name, c.Name, c.Type.SQL())
		}
	}
}

// checkReference holds a foreign key to what a document store can promise.
//
// It can promise nothing: there are no foreign keys. So a reference must SAY
// how it is stored (OnDocument), and an action the database was meant to take
// on the referencing row — cascade, set null, set default — is refused,
// because on this target nothing would take it and the rows would simply stay.
func checkReference(t *schema.Table, fk *schema.ForeignKey, add func(pos, format string, a ...any)) {
	if len(fk.Columns) != 1 {
		if fk.Document == "" {
			add(t.Pos, "%s: the foreign key (%s) must say how it is stored in a document — "+
				"t.ForeignKey(...).References(...).OnDocument(storm.Reference()) — because "+
				"embed-versus-reference is a modelling decision storm will not guess (ADR-0004)",
				t.Name, strings.Join(fk.Columns, ", "))
		}
		checkActions(t, fk, t.Pos, add)
		return
	}
	c := t.Column(fk.Columns[0])
	pos := colPos(t, c)
	var rel *schema.Relation
	for _, r := range t.Relations {
		if r.Owner && r.Column == fk.Columns[0] {
			rel = r
		}
	}
	if fk.Document == "" && (rel == nil || rel.Document == "") {
		field := fk.Columns[0]
		if rel != nil {
			field = rel.Field
		}
		add(pos, "%s.%s: a reference must say how it is stored in a document — "+
			"t.Col(&x.%s).OnDocument(storm.Reference()) — because embed-versus-reference is a "+
			"modelling decision storm will not guess (ADR-0004)", t.Name, fk.Columns[0], field)
	}
	checkActions(t, fk, pos, add)
}

func checkActions(t *schema.Table, fk *schema.ForeignKey, pos string, add func(pos, format string, a ...any)) {
	for _, a := range []schema.Action{fk.OnDelete, fk.OnUpdate} {
		switch a {
		case schema.Cascade, schema.SetNull, schema.SetDefault:
			add(pos, "%s.%s: ON DELETE/UPDATE %s — MongoDB has no foreign keys, so nothing "+
				"would perform it; do it in the write that deletes the parent",
				t.Name, strings.Join(fk.Columns, ", "), a)
		}
	}
}

func checkIndex(t *schema.Table, ix *schema.Index, add func(pos, format string, a ...any)) {
	pos := t.Pos
	if len(ix.Columns) > 0 {
		pos = colPos(t, t.Column(ix.Columns[0].Name))
	}
	name := t.IndexName(ix)
	switch {
	case ix.Where != "" && ix.LiveCol == "":
		add(pos, "%s: index %s is partial over SQL text (%s), which partialFilterExpression "+
			"cannot evaluate", t.Name, name, ix.Where)
	case ix.Method == "gin" && ginable(t, ix):
		// An array's index is multikey and a json column's a wildcard index
		// over its fields: each answers the containment GIN answers.
	case ix.Method != "" && ix.Method != "btree":
		add(pos, "%s: index %s uses %s; MongoDB indexes are B-trees", t.Name, name, ix.Method)
	case len(ix.Include) > 0:
		add(pos, "%s: index %s has INCLUDE columns; MongoDB has no covering clause", t.Name, name)
	case len(ix.With) > 0:
		add(pos, "%s: index %s has storage parameters", t.Name, name)
	}
	if ix.Unique {
		for _, k := range ix.Columns {
			if c := t.Column(k.Name); c != nil && (c.Type.Array || mongo.IsJSON(c)) {
				add(pos, "%s: unique index %s covers %s, an array or json column — MongoDB's "+
					"unique index on one constrains each ELEMENT or field, not the value",
					t.Name, name, k.Name)
			}
		}
	}
	for _, k := range ix.Columns {
		switch {
		case k.Expr:
			add(pos, "%s: index %s has an expression key (%s); MongoDB indexes fields", t.Name, name, k.Name)
		case k.OpClass != "" || k.Collate != "" || k.Prefix > 0:
			add(pos, "%s: index %s key %s has an operator class, collation or prefix length",
				t.Name, name, k.Name)
		case k.NullsFirst || k.NullsLast:
			add(pos, "%s: index %s key %s places NULLs explicitly; MongoDB's placement is fixed",
				t.Name, name, k.Name)
		}
	}
}

// ginable reports a GIN index this back end can answer: one column, an array
// or a json value.
func ginable(t *schema.Table, ix *schema.Index) bool {
	if len(ix.Columns) != 1 || ix.Columns[0].Expr {
		return false
	}
	c := t.Column(ix.Columns[0].Name)
	return c != nil && (c.Type.Array || mongo.IsJSON(c))
}

func at(pos string) string {
	if pos == "" {
		return ""
	}
	return pos + ": "
}

func colPos(t *schema.Table, c *schema.Column) string {
	if c != nil && c.Pos != "" {
		return c.Pos
	}
	return t.Pos
}
