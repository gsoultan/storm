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
		`must set Code`, // no default on the key: the caller sets it
		`{"$setOnInsert", "\"hits\":{\"$numberLong\":\"0\"}"}`, // a literal default, typed as the column
		`{"$currentDate", "\"seen\""}`,                         // the server's clock
		`func (m *Mut) SetNTimestamptzNow()`,                   // a server-side setter
		`rows[i].Code == ""`,                                   // InsertAll refuses an unset text key
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

// shaped carries every construct round one added: a composite key, arrays
// and json, soft delete with WithDeleted, a version column and a guarded
// increment.
func shaped() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{{
		Name: "stock", GoName: "Stock", PrimaryKey: []string{"branch", "id"},
		SoftDelete: "gone", ReadsDeleted: true,
		Columns: []*schema.Column{
			{Name: "branch", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true, Default: "gen_random_uuid()"},
			{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}, NotNull: true, Default: "'{}'"},
			{Name: "sizes", Type: schema.Type{Name: schema.TypeInt4, Array: true}},
			{Name: "attrs", Type: schema.Type{Name: schema.TypeJSONB}},
			{Name: "open", Type: schema.Type{Name: schema.TypeBool}, NotNull: true},
			{Name: "n", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, IncWhen: "open"},
			{Name: "maybe", Type: schema.Type{Name: schema.TypeInt4}},
			{Name: "v", Type: schema.Type{Name: schema.TypeInt4}, NotNull: true, Version: true},
			{Name: "gone", Type: schema.Type{Name: schema.TypeTimestamptz}},
		},
	}}}
}

func TestRoundOneShapesGenerateAndCompile(t *testing.T) {
	files, err := Package(shaped(), Options{Import: "github.com/gsoultan/storm"})
	if err != nil {
		t.Fatal(err)
	}
	src := files[filepath.Join("stock", "stock.gen.go")]
	s := string(src)
	for _, want := range []string{
		`\"_id\":{\"branch\":{\"$arg\":1},\"id\":{\"$arg\":2}}`, // the key subdocument
		`"_id.branch",`, // a key column's stored field
		`func MutateKey(branch string, id [16]byte) Mut`[:0] + `There is no MutateKey`, // versioned
		`func (q Query) WithDeleted() Query`,
		`func Restore(ctx context.Context, ex runtime.Executor, branch string, id [16]byte) error`,
		`func HardDelete(`,
		`func (m *Mut) IncNWhenOpen(v bool)`,
		`func (m *Mut) AddN(n int64)`,
		`func (m *Mut) IncMaybe()`,
		`func (h TextArrayCol) Overlaps(v ...string) Pred`,
		`func (h Int4ArrayCol) ContainedBy(v ...int32) Pred`,
		`func (h JSONCol) HasAllKeys(v ...string) Pred`,
		`return runtime.ErrStaleWrite`,
		`\"gone\":null`,
		`runtime.NewUUIDv4()`,
		`must set Branch`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated source lacks %s", want)
		}
	}
	for _, not := range []string{"func (m *Mut) SetV(", "func (m *Mut) AddMaybe(", "func (q Query) TagsEq("} {
		if strings.Contains(s, not) {
			t.Errorf("generated source has %s", not)
		}
	}
	if testing.Short() {
		t.Skip("-short: not building the generated package")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	dir := filepath.Join(root, "internal", "mgshaped"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "stock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stock", "stock.gen.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated package does not build:\n%s", out)
	}
}

func TestGeneratorRefusesWhatItCannotWrite(t *testing.T) {
	for name, edit := range map[string]func(*schema.Table){
		"a key that is not a column":           func(t *schema.Table) { t.PrimaryKey = []string{"nope"} },
		"no key":                               func(t *schema.Table) { t.PrimaryKey = nil },
		"a soft-delete column that is not one": func(t *schema.Table) { t.SoftDelete = "nope" },
		"a text version": func(t *schema.Table) {
			t.Columns[0].Version = true
			t.Columns[8].Version = false
		},
		"a nullable guarded counter":   func(t *schema.Table) { t.Columns[6].NotNull = false },
		"a guard that is not a column": func(t *schema.Table) { t.Columns[6].IncWhen = "nope" },
		"a guard on an array":          func(t *schema.Table) { t.Columns[6].IncWhen = "tags" },
		"an unsupported type": func(t *schema.Table) {
			t.Columns[3].Type = schema.Type{Name: schema.TypeInet}
		},
	} {
		s := shaped()
		edit(s.Tables[0])
		if _, err := File(s, s.Tables[0], "stock", "x"); err == nil {
			t.Errorf("%s: generated", name)
		}
	}
	if _, ok := kindOf(&schema.Column{Type: schema.Type{Name: schema.TypeJSONB, Array: true}}); ok {
		t.Error("an array of json is an embedded relation, not a column")
	}
	if lowerFirst("Type") != "type_" || lowerFirst("") != "" || lowerFirst("ID") != "id" ||
		lowerFirst("URLPath") != "urlPath" || lowerFirst("x") != "x" {
		t.Error("lowerFirst")
	}
}

func TestPackageRefusesBeforeWritingAnything(t *testing.T) {
	s := every()
	s.Tables[0].Checks = []*schema.Check{{Expr: "hits / 2 >= 0"}}
	if files, err := Package(s, Options{Import: "x"}); err == nil || files != nil {
		t.Fatal("a CHECK constraint that divides must be refused")
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
