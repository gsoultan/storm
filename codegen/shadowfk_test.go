package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// A relation whose foreign-key column is NOT spelled <field>_id.
//
// storm derives a relation's column as snake(field)+"_id", so CreatedBy
// normally becomes created_by_id and the generated Row has CreatedByID
// alongside the relation's CreatedBy. A real schema spells it created_by:
// then the COLUMN exports as CreatedBy too, the plan row's relation field
// shadows the embedded Row's key field, and the fetch loader read the loaded
// *Row where it meant the key. It did not compile — but only for a model with
// this shape AND a named plan over it, which no fixture had.
func shadowedFKSchema() *schema.Schema {
	users := &schema.Table{
		Name: "users", GoName: "User",
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "name", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
		},
		PrimaryKey: []string{"id"},
	}
	keys := &schema.Table{
		Name: "api_keys", GoName: "APIKey",
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			// Nullable, and named created_by rather than created_by_id.
			{Name: "created_by", Type: schema.Type{Name: schema.TypeUUID}},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "api_keys_created_by_fkey", Columns: []string{"created_by"},
			RefTable: "users", RefColumns: []string{"id"}, OnDelete: schema.SetNull,
		}},
		Relations: []*schema.Relation{{
			Field: "CreatedBy", Target: "users", TargetGo: "User",
			Column: "created_by", Owner: true, Nullable: true,
		}},
	}
	return &schema.Schema{Tables: []*schema.Table{users, keys}}
}

func TestPlanLoaderCompilesWhenTheKeyFieldIsShadowed(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated context; -short skips it")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("internal", "shadowfk"+strconv.Itoa(os.Getpid()))
	dir := filepath.Join(root, rel)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(shadowedFKSchema(), codegen.PackageOptions{
		Dir:           dir,
		Import:        "github.com/gsoultan/storm",
		Package:       "store",
		PackageImport: "github.com/gsoultan/storm/" + filepath.ToSlash(rel),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, src := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated plan loader does not compile:\n%s", out)
	}
}
