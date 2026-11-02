package mgddl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func users() *schema.Schema {
	return &schema.Schema{
		Enums: []*schema.Enum{{Name: "mood", Labels: []string{"ok", "sad"}}},
		Tables: []*schema.Table{{
			Name:       "users",
			PrimaryKey: []string{"id"},
			Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true, Default: "uuidv7()"},
				{Name: "email", Type: schema.Type{Name: schema.TypeVarchar, Size: 320}, NotNull: true},
				{Name: "nick", Type: schema.Type{Name: schema.TypeText}},
				{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}},
				{Name: "at", Type: schema.Type{Name: schema.TypeTimestamptz}, NotNull: true, Default: "now()"},
			},
			Uniques: []*schema.Unique{{Columns: []string{"email"}}, {Columns: []string{"nick"}}},
			Indexes: []*schema.Index{
				{Columns: []schema.IndexColumn{{Name: "at", Desc: true}}},
				{Name: "ix_hidden", Columns: []schema.IndexColumn{{Name: "mood"}}, Invisible: true},
				{Columns: []schema.IndexColumn{{Name: "nick"}}, Unique: true, NullsNotDistinct: true},
			},
		}},
	}
}

func TestCommandsAreJSONAndSayWhatTheModelSays(t *testing.T) {
	cmds, err := Commands(users())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cmds {
		var v map[string]any
		if err := json.Unmarshal([]byte(c), &v); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		`"required":["_id","email","at"]`,
		`"email":{"bsonType":"string","maxLength":320}`,
		`"nick":{"bsonType":["string","null"]}`,
		`"mood":{"bsonType":["string","null"],"enum":["ok","sad",null]}`,
		// A nullable unique is translated so many documents may lack it.
		`"name":"uq_users_nick","unique":true,"partialFilterExpression":{"nick":{"$type":"string"}}`,
		`"name":"uq_users_email","unique":true}`,
		`"key":{"at":-1}`,
		`"name":"ix_hidden","hidden":true`,
		// NULLS NOT DISTINCT is MongoDB's own behaviour: no filter.
		`{"key":{"nick":1},"name":"ix_users_nick","unique":true}`,
		`"validationLevel":"strict","validationAction":"error"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in\n%s", want, all)
		}
	}
	script, err := Create(users())
	if err != nil || !strings.Contains(script, "db.runCommand({\"create\":\"users\"") {
		t.Fatalf("%v\n%s", err, script)
	}
}

func TestEveryRefusalNamesItsConstruct(t *testing.T) {
	s := users()
	tb := s.Tables[0]
	tb.Pos = "model.go:1"
	tb.Columns = append(tb.Columns,
		&schema.Column{Name: "span", Type: schema.Type{Name: schema.TypeTstzRange}},
		&schema.Column{Name: "g", Type: schema.Type{Name: schema.TypeInt4}, Generated: "1"},
		&schema.Column{Name: "seq", Type: schema.Type{Name: schema.TypeInt8}, Identity: true},
		&schema.Column{Name: "v", Type: schema.Type{Name: schema.TypeInt8}, Version: true},
		&schema.Column{Name: "w", Type: schema.Type{Name: schema.TypeInt8}, IncWhen: "v"},
		&schema.Column{Name: "lc", Type: schema.Type{Name: schema.TypeText}, Default: "lower('x')"},
		&schema.Column{Name: "u", Type: schema.Type{Name: schema.TypeText}, Default: "gen_random_uuid()"},
		&schema.Column{Name: "org_id", Type: schema.Type{Name: schema.TypeUUID}, Pos: "model.go:9"},
		&schema.Column{Name: "a", Type: schema.Type{Name: schema.TypeUUID}},
	)
	tb.SoftDelete = "at"
	tb.Checks = []*schema.Check{{Expr: "lower(nick) = 'x'"}, {Expr: "id IS NOT NULL"}}
	tb.Plans = []*schema.Plan{{Name: "Tagged", Fields: []schema.PlanField{{Field: "Tags"}, {Field: "Org"}}}}
	tb.ForeignKeys = []*schema.ForeignKey{
		{Columns: []string{"org_id"}, RefTable: "orgs", OnDelete: schema.Cascade, OnUpdate: schema.Cascade, RefColumns: []string{"slug"}},
		{Columns: []string{"u"}, RefTable: "users", RefColumns: []string{"id"}, OnDelete: schema.SetDefault, Document: "reference"},
		{Columns: []string{"a", "org_id"}, RefTable: "orgs"},
	}
	tb.Relations = []*schema.Relation{{Field: "Org", Owner: true, Column: "org_id"},
		{Field: "Tags", Target: "links", ToMany: true, Link: "links"}}
	tb.Indexes = append(tb.Indexes,
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Where: "nick <> ''"},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Method: "gin"},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Method: "gist"},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Include: []string{"email"}},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, With: []schema.StorageParam{{Name: "fillfactor", Value: "70"}}},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "lower(nick)", Expr: true}}},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick", OpClass: "text_pattern_ops"}}},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick", NullsFirst: true}}},
	)
	s.Tables = append(s.Tables, &schema.Table{Name: "links", Generated: true,
		PrimaryKey: []string{"a", "b"}})
	s.Unions = []*schema.Union{{Name: "Feed"}}
	s.Functions = []*schema.Function{{Name: "f"}}
	s.Views = []*schema.View{{Name: "v"}}
	s.Triggers = []*schema.Trigger{{Name: "t"}}

	err := Check(s)
	if err == nil {
		t.Fatal("nothing refused")
	}
	for _, want := range []string{
		"users.span: type tstzrange",
		"users.g: a generated column",
		"users.seq: MongoDB has no sequence",
		"users.lc: default lower('x')",
		"users.u: a uuid default on a text column",
		"model.go:9: users.org_id: a reference must say how it is stored",
		"t.Col(&x.Org).OnDocument(storm.Reference())",
		"users.org_id: ON UPDATE CASCADE on a reference to a column that is not orgs's key",
		"users.u: ON DELETE SET DEFAULT to a generated uuid",
		"the foreign key (a, org_id) must say how it is stored",
		"CHECK (lower(nick) = 'x')", "the function lower()", "plan Tagged loads Tags through a join table",
		"is partial over SQL text", "uses gist", "INCLUDE columns", "storage parameters",
		"expression key", "operator class", "places NULLs explicitly",
		"links: a generated many-to-many",
		"union Feed", "function f", "view v", "trigger t",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q", want)
		}
	}
	if _, err := Create(s); err == nil {
		t.Error("Create must refuse what Check refuses")
	}
}

func TestADeclaredReferencePorts(t *testing.T) {
	s := users()
	tb := s.Tables[0]
	tb.Columns = append(tb.Columns, &schema.Column{Name: "org_id", Type: schema.Type{Name: schema.TypeUUID}})
	tb.ForeignKeys = []*schema.ForeignKey{{Columns: []string{"org_id"}, RefTable: "orgs", OnDelete: schema.Restrict}}
	tb.Relations = []*schema.Relation{{Field: "Org", Owner: true, Column: "org_id", Document: "reference"}}
	if err := Check(s); err != nil {
		t.Fatal(err)
	}
}

func TestTheShapesRoundOneAdded(t *testing.T) {
	text := func(name string, notNull bool) *schema.Column {
		return &schema.Column{Name: name, Type: schema.Type{Name: schema.TypeText}, NotNull: notNull}
	}
	s := &schema.Schema{
		Enums: []*schema.Enum{{Name: "mood", Labels: []string{"ok"}}},
		Tables: []*schema.Table{{
			Name:       "stock",
			PrimaryKey: []string{"branch", "sku"},
			SoftDelete: "gone",
			Columns: []*schema.Column{
				text("branch", true), text("sku", true),
				{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}, NotNull: true},
				{Name: "moods", Type: schema.Type{Name: "mood", Enum: true, Array: true}},
				{Name: "attrs", Type: schema.Type{Name: schema.TypeJSONB}, NotNull: true},
				{Name: "extra", Type: schema.Type{Name: schema.TypeJSON}},
				{Name: "v", Type: schema.Type{Name: schema.TypeInt4}, NotNull: true, Version: true},
				{Name: "n", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, IncWhen: "v"},
				text("email", false),
				{Name: "gone", Type: schema.Type{Name: schema.TypeTimestamptz}},
			},
			Indexes: []*schema.Index{
				{Columns: []schema.IndexColumn{{Name: "tags"}}, Method: "gin"},
				{Columns: []schema.IndexColumn{{Name: "attrs"}}, Method: "gin"},
				{Name: "uq_stock_email", Columns: []schema.IndexColumn{{Name: "email"}}, Unique: true,
					Where: "gone IS NULL", LiveCol: "gone"},
			},
			ForeignKeys: []*schema.ForeignKey{{Columns: []string{"branch", "sku"}, RefTable: "x",
				RefColumns: []string{"a", "b"}, Document: "reference"}},
		}},
	}
	cmds, err := Commands(s)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		`"_id":{"bsonType":"object","required":["branch","sku"],"additionalProperties":false,"properties":{"branch":{"bsonType":"string"},"sku":{"bsonType":"string"}}}`,
		`"required":["_id","tags","attrs","v","n"]`,
		`"tags":{"bsonType":"array","items":{"bsonType":"string"}}`,
		`"moods":{"bsonType":["array","null"],"items":{"bsonType":"string","enum":["ok"]}}`,
		`"attrs":{"not":{"bsonType":"null"}}`,
		`"extra":{}`,
		`{"key":{"_id.branch":1,"_id.sku":1},"name":"stock_pkey"}`,
		`{"key":{"tags":1},"name":"ix_stock_tags"}`,
		`{"key":{"attrs.$**":1},"name":"ix_stock_attrs"}`,
		`"name":"uq_stock_email","unique":true,"partialFilterExpression":{"email":{"$type":"string"},"gone":null}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in\n%s", want, all)
		}
	}
	for _, c := range cmds {
		var v map[string]any
		if err := json.Unmarshal([]byte(c), &v); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}

	// What still does not port about these shapes.
	tb := s.Tables[0]
	tb.Uniques = []*schema.Unique{{Columns: []string{"tags"}}}
	tb.Indexes = append(tb.Indexes,
		&schema.Index{Columns: []schema.IndexColumn{{Name: "attrs"}}, Unique: true},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "email"}}, Method: "gin"})
	tb.ForeignKeys[0].Document = ""
	tb.ForeignKeys[0].OnUpdate = schema.SetNull
	err = Check(s)
	for _, want := range []string{
		"stock: UNIQUE over tags", "unique index ix_stock_attrs covers attrs",
		"uses gin", "the foreign key (branch, sku) must say", "stock.branch, sku: ON UPDATE SET NULL",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q: %v", want, err)
		}
	}
}

// A delete's actions are performed by the generated package now, and ON
// UPDATE on a reference to a key no generated method can change is accepted.
func TestReferentialActionsThatPort(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{
		{Name: "orgs", PrimaryKey: []string{"a", "b"}, Columns: []*schema.Column{
			{Name: "a", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
			{Name: "b", Type: schema.Type{Name: schema.TypeText}, NotNull: true}}},
		{Name: "users", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "a", Type: schema.Type{Name: schema.TypeText}, Default: "'x'"},
			{Name: "b", Type: schema.Type{Name: schema.TypeText}}},
			ForeignKeys: []*schema.ForeignKey{{Columns: []string{"a", "b"}, RefTable: "orgs",
				RefColumns: []string{"b", "a"}, OnDelete: schema.SetDefault, OnUpdate: schema.Cascade, Document: "reference"}}},
	}}
	if err := Check(s); err != nil {
		t.Fatal(err)
	}
	s.Tables[1].ForeignKeys[0].RefColumns = []string{"a", "c"}
	if err := Check(s); err == nil {
		t.Error("ON UPDATE on a non-key reference")
	}
	s.Tables[1].ForeignKeys[0].RefColumns = []string{"a"}
	if err := Check(s); err == nil {
		t.Error("ON UPDATE on a partial key")
	}
}

func TestChecksAndPartialIndexesTranslate(t *testing.T) {
	tb := &schema.Table{Name: "items", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
		{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		{Name: "n", Type: schema.Type{Name: schema.TypeInt4}},
		{Name: "code", Type: schema.Type{Name: schema.TypeText}},
		{Name: "live", Type: schema.Type{Name: schema.TypeBool}, NotNull: true},
	}}
	tb.Checks = []*schema.Check{{Expr: "n > 0"}, {Expr: "code LIKE 'A%'"}}
	tb.Indexes = []*schema.Index{
		{Columns: []schema.IndexColumn{{Name: "code"}}, Unique: true, Where: "live"},
		{Columns: []schema.IndexColumn{{Name: "n"}}, Where: "n IN (1, 2)"},
	}
	cmds, err := Commands(&schema.Schema{Tables: []*schema.Table{tb}})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		`}},"$expr":{"$and":[{"$ne":[`,
		`"partialFilterExpression":{"$and":[{"code":{"$type":"string"}},{"live":true}]}`,
		`"partialFilterExpression":{"n":{"$in":[1,2]}}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %s:\n%s", want, all)
		}
	}
	tb.Checks = tb.Checks[:1]
	cmds, _ = Commands(&schema.Schema{Tables: []*schema.Table{tb}})
	if !strings.Contains(cmds[0], `}},"$expr":{"$ne":[`) {
		t.Errorf("one check is the $expr itself: %s", cmds[0])
	}
}
