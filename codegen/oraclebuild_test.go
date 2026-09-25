package codegen_test

// An Oracle package must carry ORACLE SQL.
//
// This is the gate the MySQL one is named after, and it earned its place the
// same way: `storm generate -dialect oracle` shipped for one commit emitting
// PostgreSQL SQL with Oracle DECODERS, and every check in place passed. The
// package compiled, it used runtime/valdec, and it read the value side of the
// port — all true, and the statements were another dialect's, because
// loweringFor had no case for the target.
//
// Compiling and EXECUTING are different claims. This asserts the second one's
// precondition without needing a server.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

type oraPortable struct {
	storm.Model
	Name  string
	Rank  int64
	Email string
}

func (o *oraPortable) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Email).Size(255)
	t.Unique(&o.Email)
}

func TestOracleGeneratedPackageCarriesOracleSQL(t *testing.T) {
	s, err := storm.Build(&oraPortable{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: codegen.DialectOracle,
	})
	if err != nil {
		t.Fatalf("generating an Oracle package: %v", err)
	}
	var all strings.Builder
	for _, b := range files {
		all.Write(b)
	}
	src := all.String()

	for _, sql := range sqlConstants(src) {
		if strings.Contains(sql, "$") {
			t.Errorf("a PostgreSQL placeholder reached Oracle SQL:\n  %s", sql)
		}
		if strings.Contains(sql, "`") || strings.Contains(sql, "[") {
			t.Errorf("another dialect's identifier quoting reached Oracle SQL:\n  %s", sql)
		}
		if strings.Contains(strings.ToUpper(sql), "RETURNING") {
			// Oracle HAS a returning clause and storm cannot use it: the INTO
			// binds output parameters runtime.Executor does not carry. One
			// emitted here would be a statement the driver cannot bind.
			t.Errorf("a returning clause reached Oracle SQL, and this target has no "+
				"usable one:\n  %s", sql)
		}
		if strings.Contains(strings.ToUpper(sql), "LIMIT ") {
			t.Errorf("LIMIT is not Oracle's row cap; FETCH FIRST is:\n  %s", sql)
		}
	}

	// And the positive: the statements must actually be Oracle's.
	//
	// The placeholder is checked as the CARRIER rather than as `:1`, because a
	// spliced statement's ordinals are assigned at run time — the source holds
	// the bare sigil and the carrier that numbers it. runtime's own test pins
	// that OraclePlaceholder renders `:1`; this pins that the generated
	// package reached for it.
	for _, want := range []string{
		`FROM "ora_portables"`,      // double quotes, which also preserve case
		"runtime.OraclePlaceholder", // not $n, not ?, not @pn
		"JSON_TABLE",                // the IN list is one bound document
		"FETCH FIRST",               // the row cap
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no %q in the generated package; the lowering may not be wired", want)
		}
	}
	for _, wrong := range []string{"MSSQLPlaceholder", "MySQLPlaceholder", "OPENJSON", "count_big"} {
		if strings.Contains(src, wrong) {
			t.Errorf("%s reached an Oracle package — this is another dialect's output "+
				"with a flag on it", wrong)
		}
	}
	// The value side of the port, chosen by the dialect at generate time.
	if !strings.Contains(src, "rows.Values()") {
		t.Error("an Oracle package must read Values: a database/sql driver decodes " +
			"before storm can see the wire")
	}
	if strings.Contains(src, "RawValues") {
		t.Error("an Oracle package must not read RawValues; it would scan nil")
	}
	// Every read asks for the value shape, once, at its Query. One that did
	// not would be reading Values off an interface that has no such method.
	queries, asked := strings.Count(src, "ex.Query("), strings.Count(src, "runtime.AsValueRows(ex.Query(")
	if queries == 0 || asked != queries {
		t.Errorf("%d of %d reads ask for runtime.ValueRows; every one must", asked, queries)
	}
	if !strings.Contains(src, "valdec.") {
		t.Error("an Oracle package must decode with runtime/valdec")
	}
}
