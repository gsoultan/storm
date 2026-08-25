package raorm_test

import (
	"context"
	"testing"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/runtime"
)

type execRecorder struct {
	sql  string
	args []any
}

func (e *execRecorder) Query(context.Context, string, []any) (runtime.Rows, error) {
	return nil, nil
}
func (e *execRecorder) Exec(_ context.Context, sql string, args []any) (int64, error) {
	e.sql, e.args = sql, args
	return 7, nil
}
func (e *execRecorder) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}
func (e *execRecorder) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return nil
}

// The exec half of the escape hatch keeps SQL[T]'s argument discipline: a
// wrong count fails at the caller with both numbers, before any bytes move.
func TestSQLExec_ArgCountAndPassThrough(t *testing.T) {
	del := raorm.SQLExec(`DELETE FROM t WHERE a = $1 AND b = $2`)
	ex := &execRecorder{}

	if _, err := del.Exec(context.Background(), ex, "x"); err == nil {
		t.Fatal("one argument for two placeholders must fail before reaching the server")
	}
	n, err := del.Exec(context.Background(), ex, "x", int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("rows affected = %d, want the executor's 7", n)
	}
	if len(ex.args) != 2 || ex.sql != `DELETE FROM t WHERE a = $1 AND b = $2` {
		t.Fatalf("statement did not pass through verbatim: %q %v", ex.sql, ex.args)
	}
}
