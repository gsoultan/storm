package codegen_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// `storm import` emits a Go MODEL, and a referential action's VALUE is SQL
// text — "SET NULL", "NO ACTION". Running that through the column-name
// humaniser produced `storm.SET NULL`, a model that does not parse; CASCADE
// and RESTRICT parsed and then failed to compile as undefined identifiers,
// which is the same defect wearing a better disguise.
//
// Found by pointing import at a real schema (argus) that uses ON DELETE SET
// NULL. Every FK in every imported model was affected.
func TestImportedActionsAreGoConstants(t *testing.T) {
	for _, a := range []schema.Action{
		schema.Cascade, schema.Restrict, schema.SetNull, schema.SetDefault,
	} {
		s := &schema.Schema{Tables: []*schema.Table{
			{
				Name:       "parents",
				GoName:     "Parent",
				Columns:    []*schema.Column{{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true}},
				PrimaryKey: []string{"id"},
			},
			{
				Name:   "children",
				GoName: "Child",
				Columns: []*schema.Column{
					{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
					{Name: "parent_id", Type: schema.Type{Name: schema.TypeUUID}},
				},
				PrimaryKey: []string{"id"},
				ForeignKeys: []*schema.ForeignKey{{
					Columns: []string{"parent_id"}, RefTable: "parents",
					RefColumns: []string{"id"}, OnDelete: a,
				}},
			},
		}}

		src, err := codegen.Model(s, codegen.ModelOptions{Package: "model", Import: "example.com/x"})
		if err != nil {
			t.Fatalf("OnDelete(%q): imported model does not parse: %v\n%s", a, err, src)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), "model.go", src, 0); err != nil {
			t.Fatalf("OnDelete(%q): %v\n%s", a, err, src)
		}
		if strings.Contains(string(src), "storm."+string(a)) {
			t.Errorf("OnDelete(%q) emitted the SQL text as an identifier:\n%s", a, src)
		}
	}
}
