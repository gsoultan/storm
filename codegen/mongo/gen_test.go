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

// every carries one column of every kind this back end generates, nullable
// and not, with a text key that has no default — the shape a uuid-keyed
// example never reaches.
func every() *schema.Schema {
	cols := []*schema.Column{{Name: "code", Type: schema.Type{Name: schema.TypeText}, NotNull: true}}
	for _, n := range []string{schema.TypeBool, schema.TypeInt2, schema.TypeInt4, schema.TypeInt8,
		schema.TypeFloat4, schema.TypeFloat8, schema.TypeVarchar, schema.TypeBytea, schema.TypeUUID,
		schema.TypeTimestamptz, schema.TypeDate, schema.TypeNumeric} {
		cols = append(cols,
			&schema.Column{Name: "n_" + n, Type: schema.Type{Name: n}, NotNull: true},
			&schema.Column{Name: "z_" + n, Type: schema.Type{Name: n}})
	}
	cols = append(cols,
		&schema.Column{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}},
		&schema.Column{Name: "hits", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, Default: "0"},
		&schema.Column{Name: "seen", Type: schema.Type{Name: schema.TypeTimestamptz}, NotNull: true, Default: "now()", Immutable: true})
	return &schema.Schema{
		Enums:  []*schema.Enum{{Name: "mood", Labels: []string{"ok"}}},
		Tables: []*schema.Table{{Name: "things", GoName: "Thing", PrimaryKey: []string{"code"}, Columns: cols}},
	}
}

func TestEveryKindGeneratesAndCompiles(t *testing.T) {
	files, err := Package(every(), Options{Import: "github.com/gsoultan/storm"})
	if err != nil {
		t.Fatal(err)
	}
	src, ok := files[filepath.Join("thing", "thing.gen.go")]
	if !ok {
		t.Fatalf("files: %v", files)
	}
	s := string(src)
	for _, want := range []string{
		`must set Code`,                      // no default on the key: the caller sets it
		`{"$setOnInsert", "\"hits\":0"}`,     // a literal default
		`{"$currentDate", "\"seen\""}`,       // the server's clock
		`func (m *Mut) SetNTimestamptzNow()`, // a server-side setter
		`rows[i].Code == ""`,                 // InsertAll refuses an unset text key
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated source lacks %s", want)
		}
	}
	if strings.Contains(s, "func (m *Mut) SetSeen(") {
		t.Error("an Immutable column has no update setter")
	}
	again, _ := Package(every(), Options{Import: "github.com/gsoultan/storm"})
	if string(again[filepath.Join("thing", "thing.gen.go")]) != s {
		t.Error("generation is not deterministic")
	}

	// It must COMPILE: an assertion on text cannot tell a package that
	// builds from one that does not.
	if testing.Short() {
		t.Skip("-short: not building the generated package")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	dir := filepath.Join(root, "internal", "mgevery"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "thing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "thing", "thing.gen.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated package does not build:\n%s", out)
	}
}

func TestPackageRefusesBeforeWritingAnything(t *testing.T) {
	s := every()
	s.Tables[0].Checks = []*schema.Check{{Expr: "hits >= 0"}}
	if files, err := Package(s, Options{Import: "x"}); err == nil || files != nil {
		t.Fatal("a CHECK constraint must be refused")
	}
	if _, err := Package(every(), Options{}); err == nil {
		t.Error("Import is required")
	}
	if _, err := Package(every(), Options{Import: "x", Only: []string{"nope"}}); err == nil {
		t.Error("an unknown table")
	}
	two := every()
	dup := *two.Tables[0]
	dup.Name = "things2"
	two.Tables = append(two.Tables, &dup)
	if _, err := Package(two, Options{Import: "x"}); err == nil || !strings.Contains(err.Error(), "both generate") {
		t.Errorf("two tables in one package: %v", err)
	}
	wide := every()
	for i := 0; i < 64; i++ {
		wide.Tables[0].Columns = append(wide.Tables[0].Columns,
			&schema.Column{Name: "w" + strconv.Itoa(i), Type: schema.Type{Name: schema.TypeInt4}})
	}
	if _, err := Package(wide, Options{Import: "x"}); err == nil || !strings.Contains(err.Error(), "64") {
		t.Errorf("a table past the mask: %v", err)
	}
}
