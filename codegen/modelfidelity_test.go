package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// What an imported model must not quietly lose.
//
// Each case here was a real DROP or ALTER proposed against a live database by
// a model `storm import` had just written from it. They share a failure shape:
// the model PARSED, COMPILED and looked right, and meant something narrower
// than the schema it came from — so only a diff against the source could tell.

func fidelitySchema() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{
		{
			Name: "tenants", GoName: "Tenant",
			Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true, Default: "gen_random_uuid()"},
			},
			PrimaryKey: []string{"id"},
		},
		{
			Name: "identities", GoName: "Identity",
			Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true, Default: "gen_random_uuid()"},
				{Name: "tenant_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			},
			PrimaryKey:  []string{"id"},
			Uniques:     []*schema.Unique{{Name: "identities_id_tenant_id_key", Columns: []string{"id", "tenant_id"}}},
			ForeignKeys: []*schema.ForeignKey{{Name: "identities_tenant_id_fkey", Columns: []string{"tenant_id"}, RefTable: "tenants", RefColumns: []string{"id"}}},
		},
		{
			Name: "audit", GoName: "Audit",
			Columns: []*schema.Column{
				// A composite primary key, which nothing infers.
				{Name: "tenant_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
				{Name: "identity_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
				// NOT NULL bytea: the Go type cannot carry the constraint.
				{Name: "entry_hash", Type: schema.Type{Name: schema.TypeBytea}, NotNull: true},
				// A key with no default: this table must NOT embed storm.Model.
				{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			},
			PrimaryKey: []string{"tenant_id", "identity_id"},
			ForeignKeys: []*schema.ForeignKey{{
				Name:     "audit_identity_tenant_fkey",
				Columns:  []string{"identity_id", "tenant_id"},
				RefTable: "identities", RefColumns: []string{"id", "tenant_id"},
				OnDelete: schema.Cascade,
			}},
		},
	}}
}

func importedSource(t *testing.T) string {
	t.Helper()
	src, err := codegen.Model(fidelitySchema(), codegen.ModelOptions{Package: "model"})
	if err != nil {
		t.Fatalf("imported model does not parse: %v\n%s", err, src)
	}
	return string(src)
}

// A composite foreign key has no relation field to carry it. Dropped, a child
// row in one tenant may reference a parent in another — the constraint is the
// tenant boundary, not a detail of it.
func TestImportCarriesCompositeForeignKeys(t *testing.T) {
	src := importedSource(t)
	if !strings.Contains(src, "t.ForeignKey(") || !strings.Contains(src, ".References(") {
		t.Fatalf("composite foreign key was dropped:\n%s", src)
	}
	if !strings.Contains(src, "audit_identity_tenant_fkey") {
		t.Errorf("the constraint name was lost, so a diff would rename it:\n%s", src)
	}
	if !strings.Contains(src, "storm.Cascade") {
		t.Errorf("ON DELETE CASCADE was lost:\n%s", src)
	}
}

// A composite primary key is not inferable from any field.
func TestImportCarriesCompositePrimaryKeys(t *testing.T) {
	if src := importedSource(t); !strings.Contains(src, "t.PrimaryKey(") {
		t.Fatalf("composite primary key was dropped:\n%s", src)
	}
}

// []byte is read as nullable, so NOT NULL has to be said out loud.
func TestImportCarriesNotNullOnBytea(t *testing.T) {
	if src := importedSource(t); !strings.Contains(src, ".NotNull()") {
		t.Fatalf("NOT NULL on a bytea column was dropped:\n%s", src)
	}
}

// storm indexes every foreign key; a database that does not must say so, or
// every diff proposes indexes the source schema never had.
func TestImportSuppressesIndexesTheSourceDoesNotHave(t *testing.T) {
	if src := importedSource(t); !strings.Contains(src, ".NoIndex()") {
		t.Fatalf("no NoIndex: the model would add indexes the database lacks:\n%s", src)
	}
}

// storm.Model brings defaults, not just columns. A table whose key has no
// default must not embed it, or the model silently gains gen_random_uuid().
func TestImportDoesNotEmbedModelWhenDefaultsDiffer(t *testing.T) {
	src := importedSource(t)
	i := strings.Index(src, "type Audit struct")
	if i < 0 {
		t.Fatalf("Audit not emitted:\n%s", src)
	}
	body := src[i:]
	if j := strings.Index(body, "}"); j >= 0 {
		body = body[:j]
	}
	if strings.Contains(body, "storm.Model") {
		t.Fatalf("Audit embeds storm.Model, which would give its key a default it does not have:\n%s", body)
	}
}
