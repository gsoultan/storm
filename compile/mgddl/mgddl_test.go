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
	tb.Checks = []*schema.Check{{Expr: "1=1"}}
	tb.Plans = []*schema.Plan{{}}
	tb.ForeignKeys = []*schema.ForeignKey{
		{Columns: []string{"org_id"}, RefTable: "orgs", OnDelete: schema.Cascade},
		{Columns: []string{"a", "org_id"}, RefTable: "orgs"},
	}
	tb.Relations = []*schema.Relation{{Field: "Org", Owner: true, Column: "org_id"}}
	tb.Indexes = append(tb.Indexes,
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Where: "nick <> ''"},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "nick"}}, Method: "gin"},
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
		"model.go:1: users: soft delete",
		"users.span: type tstzrange",
		"users.g: a generated column",
		"users.seq: MongoDB has no sequence",
		"users.v: version columns",
		"users.w: IncWhen",
		"users.lc: default lower('x')",
		"users.u: a uuid default on a text column",
		"model.go:9: users.org_id: a reference must say how it is stored",
		"t.Col(&x.Org).OnDocument(storm.Reference())",
		"ON DELETE/UPDATE CASCADE",
		"composite foreign key (a, org_id)",
		"CHECK constraints", "named plans",
		"is partial over SQL text", "uses gin", "INCLUDE columns", "storage parameters",
		"expression key", "operator class", "places NULLs explicitly",
		"links: needs a single-column primary key", "links: a generated many-to-many",
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
