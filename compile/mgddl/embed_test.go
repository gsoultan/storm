package mgddl

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

// orders hold their lines.
func embedded() *schema.Schema {
	uuid := schema.Type{Name: schema.TypeUUID}
	return &schema.Schema{Tables: []*schema.Table{
		{Name: "orders", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
			{Name: "id", Type: uuid, NotNull: true}}},
		{Name: "lines", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
			{Name: "id", Type: uuid, NotNull: true},
			{Name: "order_id", Type: uuid, NotNull: true},
			{Name: "sku", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
			{Name: "qty", Type: schema.Type{Name: schema.TypeInt4}}},
			ForeignKeys: []*schema.ForeignKey{{Columns: []string{"order_id"}, RefTable: "orders",
				RefColumns: []string{"id"}, OnDelete: schema.Cascade}},
			Relations: []*schema.Relation{{Field: "Order", Target: "orders", Owner: true, Column: "order_id", Document: "embed"}},
			Indexes: []*schema.Index{
				{Columns: []schema.IndexColumn{{Name: "order_id"}}},
				{Columns: []schema.IndexColumn{{Name: "sku"}, {Name: "qty", Desc: true}}},
			}},
	}}
}

func TestEmbeddedTablesLiveInTheirParent(t *testing.T) {
	cmds, err := Commands(embedded())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		`"lines":{"bsonType":"array","items":{"bsonType":"object","required":["_id","sku"],"properties":{"_id":{"bsonType":"binData"},"sku":{"bsonType":"string"},"qty":{"bsonType":["int","null"]}}}}`,
		`{"key":{"lines.sku":1,"lines.qty":-1},"name":"ix_lines_sku_qty"}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %s:\n%s", want, all)
		}
	}
	if strings.Contains(all, `"create":"lines"`) || strings.Contains(all, `"lines.order_id"`) || strings.Contains(all, `"order_id"`) {
		t.Errorf("an embedded table has no collection, and its elements no reference:\n%s", all)
	}
}

func TestEmbeddingRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(s *schema.Schema)
		want string
	}{
		"a nullable reference": {func(s *schema.Schema) { s.Tables[1].Columns[1].NotNull = false }, "must be NOT NULL"},
		"no cascade":           {func(s *schema.Schema) { s.Tables[1].ForeignKeys[0].OnDelete = "" }, "OnDelete(storm.Cascade)"},
		"not the parent's key": {func(s *schema.Schema) { s.Tables[1].ForeignKeys[0].RefColumns = []string{"code"} }, "must be to orders's key"},
		"a composite key":      {func(s *schema.Schema) { s.Tables[1].PrimaryKey = []string{"id", "sku"} }, "single-column key"},
		"two parents": {func(s *schema.Schema) {
			s.Tables[1].Relations = append(s.Tables[1].Relations, &schema.Relation{Field: "Other", Target: "orders", Owner: true, Column: "id", Document: "embed"})
		}, "embedded in two parents"},
		"another reference": {func(s *schema.Schema) {
			s.Tables[1].Relations = append(s.Tables[1].Relations, &schema.Relation{Field: "Other", Target: "orders", Owner: true, Column: "id", Document: "reference"})
		}, "refers elsewhere"},
		"nested": {func(s *schema.Schema) {
			s.Tables[0].Columns = append(s.Tables[0].Columns, &schema.Column{Name: "x", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true})
			s.Tables[0].Relations = []*schema.Relation{{Field: "X", Target: "lines", Owner: true, Column: "x", Document: "embed"}}
			s.Tables[0].ForeignKeys = []*schema.ForeignKey{{Columns: []string{"x"}, RefTable: "lines", RefColumns: []string{"id"}, OnDelete: schema.Cascade}}
		}, "itself embedded"},
		"pointed at": {func(s *schema.Schema) {
			s.Tables = append(s.Tables, &schema.Table{Name: "notes", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true}},
				ForeignKeys: []*schema.ForeignKey{{Columns: []string{"id"}, RefTable: "lines", Document: "reference"}}})
		}, "an element cannot be pointed at"},
		"soft delete": {func(s *schema.Schema) {
			s.Tables[1].SoftDelete = "qty"
		}, "soft delete"},
		"a unique index": {func(s *schema.Schema) { s.Tables[1].Indexes[1].Unique = true }, "a unique index"},
		"a version":      {func(s *schema.Schema) { s.Tables[1].Columns[3].Version = true }, "a version column"},
		"a projection": {func(s *schema.Schema) {
			s.Tables[1].Projections = []*schema.Projection{{Name: "P", Columns: []string{"sku"}}}
		}, "projections"},
		"a check": {func(s *schema.Schema) { s.Tables[1].Checks = []*schema.Check{{Expr: "qty > 0"}} }, "a CHECK constraint"},
	} {
		s := embedded()
		c.edit(s)
		if err := Check(s); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
