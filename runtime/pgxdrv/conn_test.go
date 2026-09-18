package pgxdrv_test

// The pinned-connection adapter's paths that a live test does not reach.
//
// conn_live_test.go covers what Conn EXISTS for — a session-scoped advisory
// lock, which a pool cannot hold. What it cannot reach is the failure side:
// every method classifies the driver's error, and an unclassified one is an
// error a handler cannot tell a 409 from a 500 by. A fake connection reaches
// those directly, and needs no server to do it.

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// failing is a PgxConn that answers everything with one error.
type failing struct {
	err      error
	tag      pgconn.CommandTag
	sendCall int
}

func (f *failing) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, f.err
}
func (f *failing) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return f.tag, f.err
}
func (f *failing) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, f.err
}
func (f *failing) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	f.sendCall++
	return nil
}

func unique() error {
	return &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key", TableName: "users"}
}

// A constraint violation must arrive in storm's own vocabulary here as well as
// through Pool. The adapter a caller reaches for is the one they pinned, and a
// handler written against Pool's errors has to keep working.
func TestConnClassifiesEveryMethodsError(t *testing.T) {
	ctx := context.Background()
	c := pgxdrv.Conn{C: &failing{err: unique()}}

	if _, err := c.Query(ctx, "SELECT 1", nil); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("Query: err = %v, want a unique violation", err)
	}
	if _, err := c.Exec(ctx, "INSERT", nil); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("Exec: err = %v, want a unique violation", err)
	}
	if _, err := c.CopyFrom(ctx, "users", []string{"id"}, &emptySource{}); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("CopyFrom: err = %v, want a unique violation", err)
	}

	// ...and the metadata the classification carries, which is what turns a
	// 409 into one naming the field the caller has to fix.
	var ce *runtime.ConstraintError
	_, err := c.Exec(ctx, "INSERT", nil)
	if !errors.As(err, &ce) {
		t.Fatalf("Exec: err = %v, want a *runtime.ConstraintError", err)
	}
	if ce.Constraint != "users_email_key" || ce.Table != "users" {
		t.Errorf("the classification lost its metadata: %+v", ce)
	}
}

// An error storm has no opinion about passes through UNCHANGED. A wrapper that
// renamed every error would hide the ones worth reading verbatim.
func TestConnPassesAnUnrecognisedErrorThrough(t *testing.T) {
	mine := errors.New("the network went away")
	c := pgxdrv.Conn{C: &failing{err: mine}}
	if _, err := c.Query(context.Background(), "SELECT 1", nil); !errors.Is(err, mine) {
		t.Errorf("err = %v, want the driver's own error", err)
	}
}

func TestConnExecReportsTheAffectedCount(t *testing.T) {
	c := pgxdrv.Conn{C: &failing{tag: pgconn.NewCommandTag("UPDATE 3")}}
	n, err := c.Exec(context.Background(), "UPDATE users SET x = 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
}

// An empty batch sends NOTHING. Queueing zero statements and draining the
// result would be a round trip for no work, and pgx's SendBatch on an empty
// batch is not a shape worth relying on.
func TestConnBatchWithNoOpsSendsNothing(t *testing.T) {
	f := &failing{}
	c := pgxdrv.Conn{C: f}
	called := false
	err := c.Batch(context.Background(), nil, func(int, runtime.Rows, int64, error) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("an empty batch returned %v", err)
	}
	if f.sendCall != 0 {
		t.Error("an empty batch reached the connection")
	}
	if called {
		t.Error("the callback ran for a batch with no ops")
	}
}

type emptySource struct{}

func (emptySource) Next() bool    { return false }
func (emptySource) Values() []any { return nil }
func (emptySource) Err() error    { return nil }
