package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

type portsModel struct {
	storm.Model
	Hostname string
	// integer[] — what the second adopter's `agents` table actually holds.
	// storm had int8[], text[], uuid[] and numeric[], and for this one its
	// only answer was to omit the column, after which the first `storm diff`
	// proposed dropping it.
	SSHPorts []int32
	Wide     []int64
}

func (m *portsModel) Schema(t *storm.Table) { t.Unique(&m.Hostname) }

// A new column type is not done when it infers. It is done when the generated
// package COMPILES: the kind reaches nine per-kind maps, and the file's own
// comment records that three of them had already been missed once each while
// adding a type. This is the check that makes the tenth cheap.
func TestInt4ArrayGeneratesAndCompiles(t *testing.T) {
	s, err := storm.Build(&portsModel{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("ports_models")
	if tbl == nil {
		t.Fatal("model did not build a table")
	}
	col := tbl.Column("ssh_ports")
	if col == nil {
		t.Fatal("ssh_ports is missing")
	}
	if !col.Type.Array || col.Type.Name != "int4" {
		t.Fatalf("ssh_ports typed %s, want int4[]", col.Type.SQL())
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "int4build"+strconv.Itoa(os.Getpid()))
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated package with an int4[] column does not compile:\n%s", out)
	}
}
