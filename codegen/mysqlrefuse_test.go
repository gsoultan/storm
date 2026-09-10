package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// The hole this closes: joins, aggregates, unions, top-N and recursive reads
// still went through compile/pgsql whichever dialect was asked for, so a MySQL
// model declaring one got PostgreSQL SQL SILENTLY — the original M9 defect,
// unfixed for exactly the constructs nobody had generated yet.
//
// They refuse now. A refusal is a compile error with a name in it; the
// alternative is a statement the server rejects at run time, or worse accepts.
type myRecursive struct {
	storm.Model
	Name   string
	Parent *myRecursive
}

func (m *myRecursive) Schema(t *storm.Table) { t.Col(&m.Name).Size(80) }

func TestUnloweredConstructsRefuseRatherThanEmitPostgres(t *testing.T) {
	s, err := storm.Build(&myRecursive{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: codegen.DialectMySQL,
	})
	if err == nil {
		t.Fatal("a self-referential model generated for MySQL; its recursive read would be PostgreSQL SQL")
	}
	for _, want := range []string{"recursive read", "mysql", "storm.SQL", "M9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	// The same model is fine on PostgreSQL, so this is a dialect gap and not a
	// model storm cannot express.
	if _, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
	}); err != nil {
		t.Fatalf("the PostgreSQL path was caught by a MySQL refusal: %v", err)
	}
}
