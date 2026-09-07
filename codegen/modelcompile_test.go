package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// argusLike is the shape that broke the on-ramp: a jsonb column, and an index
// over a foreign-key column.
//
// Both survived because the only check on an imported model was that it
// PARSES. A model that parses can still name a type it does not import and a
// field that does not exist, and this one named both.
func argusLike() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{
		{
			Name: "users", GoName: "User",
			Columns:    []*schema.Column{{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true}},
			PrimaryKey: []string{"id"},
		},
		{
			Name: "recovery_codes", GoName: "RecoveryCode",
			Columns: []*schema.Column{
				{Name: "user_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
				{Name: "posture", Type: schema.Type{Name: schema.TypeJSONB}},
				{Name: "amount", Type: schema.Type{Name: schema.TypeNumeric, Precision: 18, Scale: 4}},
				{Name: "used_at", Type: schema.Type{Name: schema.TypeTimestamptz}},
			},
			PrimaryKey: []string{"user_id"},
			ForeignKeys: []*schema.ForeignKey{{
				Columns: []string{"user_id"}, RefTable: "users",
				RefColumns: []string{"id"}, OnDelete: schema.Cascade,
			}},
			Indexes: []*schema.Index{{
				Name: "recovery_codes_user_idx", Method: "btree",
				Columns: []schema.IndexColumn{{Name: "user_id"}},
				Where:   "used_at IS NULL",
			}},
		},
	}}
}

// An imported model has to COMPILE, which is a different claim from parsing
// and the only one an adopter cares about: the first thing they do with the
// output is build it.
func TestImportedModelCompiles(t *testing.T) {
	src, err := codegen.Model(argusLike(), codegen.ModelOptions{Package: "model"})
	if err != nil {
		t.Fatalf("imported model does not parse: %v\n%s", err, src)
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "modelbuild"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "model.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("imported model does not compile:\n%s\n--- model ---\n%s", out, src)
	}

	// A model file may import stdlib and storm. runtime is storm's internal
	// decode surface; naming it here means the emitter reached for the type
	// table generated code uses, which the model cannot import.
	if strings.Contains(string(src), "runtime.") {
		t.Errorf("model names runtime, which it does not import:\n%s", src)
	}
}
