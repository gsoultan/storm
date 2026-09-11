package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// The hole this closes: joins, aggregates, unions, top-N and recursive reads
// went through compile/pgsql whichever dialect was asked for, so a MySQL model
// declaring one got PostgreSQL SQL SILENTLY — the original M9 defect, unfixed
// for exactly the constructs nobody had generated yet.
//
// Four of the five are lowered now. What is left refuses, and a refusal is a
// compile error with a name in it; the alternative is a statement the server
// rejects at run time, or worse accepts.
type myRecursive struct {
	storm.Model
	Name   string
	Parent *myRecursive
}

func (m *myRecursive) Schema(t *storm.Table) { t.Col(&m.Name).Size(80) }

// A self-referential model used to be refused for MySQL, because its recursive
// read would have been PostgreSQL SQL. It generates now — the cycle guard is a
// HEX path and FIND_IN_SET rather than an array, since MySQL has no arrays.
func TestSelfReferentialModelGeneratesForMySQL(t *testing.T) {
	s, err := storm.Build(&myRecursive{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: codegen.DialectMySQL,
	})
	if err != nil {
		t.Fatalf("a self-referential model was refused for MySQL: %v", err)
	}
	var all strings.Builder
	for _, b := range files {
		all.Write(b)
	}
	src := all.String()
	if !strings.Contains(src, "WITH RECURSIVE") {
		t.Fatal("no recursive read was emitted; the fixture is wrong")
	}
	for _, banned := range []string{"ARRAY[", "= ANY(", `FROM "my_recursives"`} {
		if strings.Contains(src, banned) {
			t.Errorf("PostgreSQL SQL reached the MySQL recursive read: %s", banned)
		}
	}
	if !strings.Contains(src, "FIND_IN_SET") {
		t.Error("the cycle guard is not the FIND_IN_SET form; MySQL has no arrays")
	}
}
