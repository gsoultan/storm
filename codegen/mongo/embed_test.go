package codegenmongo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

// withLines is related() plus lines embedded in orgs, with a server-clock
// column and a key the caller sets.
func withLines(key string) *schema.Schema {
	s := related()
	s.Tables[0].Relations = append(s.Tables[0].Relations, &schema.Relation{Field: "Lines", Target: "lines", ToMany: true, Column: "org_id"},
		&schema.Relation{Field: "Main", Target: "lines", Column: "org_id"})
	s.Tables[0].Plans = append(s.Tables[0].Plans, &schema.Plan{Name: "Ledger", Fields: []schema.PlanField{{Field: "Lines"}, {Field: "Main"}}})
	s.Tables = append(s.Tables, &schema.Table{Name: "lines", GoName: "Line", PrimaryKey: []string{"n"}, Columns: []*schema.Column{
		{Name: "n", Type: schema.Type{Name: key}, NotNull: true},
		{Name: "org_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		{Name: "at", Type: schema.Type{Name: schema.TypeTimestamptz}, NotNull: true, Default: "now()"},
		{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}},
		{Name: "note", Type: schema.Type{Name: schema.TypeText}},
	},
		ForeignKeys: []*schema.ForeignKey{{Columns: []string{"org_id"}, RefTable: "orgs", RefColumns: []string{"id"}, OnDelete: schema.Cascade}},
		Relations:   []*schema.Relation{{Field: "Org", Target: "orgs", Owner: true, Column: "org_id", Document: "embed"}}})
	return s
}

func TestEmbeddedGeneratesAndCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: not building the generated packages")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	for _, key := range []string{schema.TypeText, schema.TypeUUID} {
		base := "mgembed" + key + strconv.Itoa(os.Getpid())
		dir := filepath.Join(root, "internal", base)
		t.Cleanup(func() { os.RemoveAll(dir) })
		s := withLines(key)
		if key == schema.TypeUUID {
			s.Tables[3].Columns[0].Default = "uuidv7()"
		}
		files, err := Package(s, Options{Import: "github.com/gsoultan/storm", Package: base,
			PackageImport: "github.com/gsoultan/storm/internal/" + base})
		if err != nil {
			t.Fatal(err)
		}
		src := string(files[filepath.Join("line", "line.gen.go")])
		for _, want := range []string{
			`\"$ifNull\":[{\"$literal\":{\"$arg\":3}},\"$$NOW\"]`,
			"zeroNil(r.At)",
			"func Load(ctx context.Context, ex runtime.Executor, parents ...[16]byte)",
		} {
			if !strings.Contains(src, want) {
				t.Errorf("%s: lacks %s", key, want)
			}
		}
		if ctx := string(files[base+".gen.go"]); !strings.Contains(ctx, "line.Load(ctx, ex, keys...)") || strings.Contains(ctx, "LineWithOrg") {
			t.Errorf("%s: an embedded relation loads through Load, and starts no plan", key)
		}
		if org := string(files[filepath.Join("org", "org.gen.go")]); strings.Contains(org, `"lines"`) {
			t.Errorf("a parent's delete does nothing to its embedded rows but delete them with it")
		}
		for rel, b := range files {
			full := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("go", "vet", "./internal/"+base+"/...")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: the generated packages do not build:\n%s", key, out)
		}
	}
}

func TestEmbeddedRefuses(t *testing.T) {
	s := withLines(schema.TypeText)
	s.Tables[3].Columns[1].NotNull = false
	if _, err := FileEmbedded(s, s.Tables[3], "line", "x"); err == nil || !strings.Contains(err.Error(), "NOT NULL copy") {
		t.Errorf("a nullable reference: %v", err)
	}
	s = withLines(schema.TypeText)
	s.Tables[3].PrimaryKey = []string{"n", "at"}
	if _, err := FileEmbedded(s, s.Tables[3], "line", "x"); err == nil || !strings.Contains(err.Error(), "single-column key") {
		t.Errorf("a composite key: %v", err)
	}
	s = withLines(schema.TypeBytea)
	if _, err := FileEmbedded(s, s.Tables[3], "line", "x"); err == nil || !strings.Contains(err.Error(), "comparable") {
		t.Errorf("a bytea key: %v", err)
	}
	s = withLines(schema.TypeText)
	s.Tables[3].Columns[3].Type = schema.Type{Name: schema.TypeInet}
	if _, err := FileEmbedded(s, s.Tables[3], "line", "x"); err == nil {
		t.Error("an unsupported column")
	}
	s = withLines(schema.TypeText)
	s.Tables[0].Plans[1].Fields[0].Nested = []schema.PlanField{{Field: "Org"}}
	if _, err := plans(s, []string{"orgs", "users", "profiles", "lines"}); err == nil || !strings.Contains(err.Error(), "embedded") {
		t.Errorf("a plan through an embedded table: %v", err)
	}
}
