package pgxdrv_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every one of these is a 4xx a service has to tell from a 500. Left
// unclassified they are all the same opaque driver error, and the handler has
// to type-assert a pgx type and decode a SQLSTATE — driver knowledge leaking
// through the port that exists to stop it.
func TestConstraintViolationsAreClassified(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	const ns = "storm_constraint_test"
	mustExec(t, pool, `DROP SCHEMA IF EXISTS `+ns+` CASCADE; CREATE SCHEMA `+ns)
	defer pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+ns+` CASCADE`)
	mustExec(t, pool, `SET search_path TO `+ns+`;
		CREATE TABLE parent(id int PRIMARY KEY);
		CREATE TABLE child(
			id    int PRIMARY KEY,
			pid   int REFERENCES parent(id),
			email text UNIQUE,
			n     int NOT NULL,
			CONSTRAINT n_positive CHECK (n > 0));
		CREATE EXTENSION IF NOT EXISTS btree_gist;
		CREATE TABLE booking(
			room int,
			during tstzrange,
			EXCLUDE USING gist (room WITH =, during WITH &&));
		INSERT INTO parent VALUES (1);
		INSERT INTO child VALUES (1, 1, 'a@x', 1);`)

	q := func(sql string) error {
		_, err := ex.Exec(ctx, `SET search_path TO `+ns+`; `+sql, nil)
		return err
	}

	cases := []struct {
		name string
		sql  string
		want error
	}{
		{"unique", `INSERT INTO child VALUES (2, 1, 'a@x', 1)`, runtime.ErrUniqueViolation},
		{"foreign key", `INSERT INTO child VALUES (3, 999, 'b@x', 1)`, runtime.ErrForeignKeyViolation},
		{"check", `INSERT INTO child VALUES (4, 1, 'c@x', 0)`, runtime.ErrCheckViolation},
		{"not null", `INSERT INTO child(id, pid, email) VALUES (5, 1, 'd@x')`, runtime.ErrNotNullViolation},
		{"exclusion", `INSERT INTO booking VALUES (1, '[2026-01-01,2026-02-01)'), (1, '[2026-01-15,2026-03-01)')`,
			runtime.ErrExclusionViolation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := q(c.sql)
			if err == nil {
				t.Fatal("the statement succeeded; it was supposed to violate a constraint")
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("errors.Is(err, %v) is false: %v", c.want, err)
			}
			// The constraint's NAME must survive, so a handler can say which.
			var ce *runtime.ConstraintError
			if !errors.As(err, &ce) {
				t.Fatalf("not a ConstraintError: %v", err)
			}
			if ce.Constraint == "" && ce.Table == "" && ce.Column == "" {
				t.Error("nothing identifies which constraint was violated")
			}
		})
	}
}

// storm's own error text must not carry a bound VALUE. PostgreSQL's does
// ("Key (email)=(a@x) already exists"), that message belongs to the server, and
// it stays reachable through Unwrap — but it is not folded into storm's text,
// so logging a storm error cannot leak what logging the driver's would.
func TestConstraintErrorDoesNotCarryValues(t *testing.T) {
	pg := &runtime.ConstraintError{
		Kind:       runtime.ErrUniqueViolation,
		Constraint: "child_email_key",
		Table:      "child",
		Column:     "email",
		Err:        errors.New(`ERROR: duplicate key value violates unique constraint "child_email_key" (SQLSTATE 23505) Key (email)=(ada@example.com) already exists`),
	}
	if strings.Contains(pg.Error(), "ada@example.com") {
		t.Errorf("storm's error text carries a bound value: %s", pg.Error())
	}
	if !strings.Contains(pg.Error(), "child_email_key") {
		t.Errorf("storm's error text does not name the constraint: %s", pg.Error())
	}
	// The server's message is still one Unwrap away.
	if !strings.Contains(errors.Unwrap(pg).Error(), "ada@example.com") {
		t.Error("the server's diagnostic was discarded")
	}
}

// An error storm has no opinion about must come back UNCHANGED. Renaming
// everything would hide exactly the errors worth reading verbatim.
func TestUnknownErrorsPassThrough(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	_, err = ex.Exec(ctx, `SELECT * FROM a_table_that_does_not_exist`, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var ce *runtime.ConstraintError
	if errors.As(err, &ce) {
		t.Errorf("an unrelated error was classified as a constraint violation: %v", err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("the server's message was lost: %v", err)
	}
}

// Retryable separates "the client's problem" from "nobody's problem, run it
// again".
func TestRetryable(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{runtime.ErrSerializationFailure, true},
		{runtime.ErrDeadlock, true},
		{runtime.ErrUniqueViolation, false},
		{errors.New("something else"), false},
		{&runtime.ConstraintError{Kind: runtime.ErrDeadlock}, true},
		{&runtime.ConstraintError{Kind: runtime.ErrUniqueViolation}, false},
	} {
		if got := runtime.Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

// The tstzrange parameter codec. Encoding it as a real tstzrange is what makes
// `WHERE during && $1` resolvable: a prepared statement fixes the parameter's
// type before the value exists, so an untyped text value would be resolved as
// text and the operator would not be found.
func TestTstzRangeParameterBinds(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	lo := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	r := runtime.NewTstzRange(lo, hi)

	// Bound as a range, compared with a range operator.
	rows, err := ex.Query(ctx, `SELECT $1::tstzrange && tstzrange($2, $3, '[)')`,
		[]any{r, lo.Add(time.Hour), hi.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	if !runtime.Bool(rows.RawValues()[0]) {
		t.Error("[09,11) and [10,12) must overlap")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// And the value survives the round trip through the server.
	back, err := ex.Query(ctx, `SELECT $1::tstzrange`, []any{r})
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if !back.Next() {
		t.Fatal("no row")
	}
	got, err := runtime.DecodeTstzRange(back.RawValues()[0])
	if err != nil {
		t.Fatal(err)
	}
	if !got.Lower.Equal(lo) || !got.Upper.Equal(hi) || !got.LowerInc || got.UpperInc {
		t.Errorf("round trip changed the range: %+v", got)
	}
}

// A nil *TstzRange is SQL NULL, not a zero range.
func TestNilRangePointerIsNull(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	var r *runtime.TstzRange
	rows, err := ex.Query(ctx, `SELECT $1::tstzrange IS NULL`, []any{r})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	if !runtime.Bool(rows.RawValues()[0]) {
		t.Error("a nil *TstzRange did not bind as NULL")
	}
}

// A caller who only wants to know whether a bulk insert worked passes no
// callback. That used to be a nil function call: the first result took the
// connection with it and the symptom was a hung test binary rather than an
// error naming the batch — which is the worst way to learn that an
// implementation of a port assumed something the port did not promise.
func TestBatch_NilCallbackDrainsAndReportsTheFirstError(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	ns := fmt.Sprintf("storm_nilbatch_%d", os.Getpid())
	mustExec(t, pool, `DROP SCHEMA IF EXISTS `+ns+` CASCADE; CREATE SCHEMA `+ns)
	defer pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+ns+` CASCADE`)
	mustExec(t, pool, `SET search_path TO `+ns+`;
		CREATE TABLE nb (id int PRIMARY KEY, n int NOT NULL);`)

	ins := func(id, n int) runtime.BatchOp {
		return runtime.BatchOp{
			SQL:  `INSERT INTO ` + ns + `.nb (id, n) VALUES ($1, $2)`,
			Args: []any{id, n},
		}
	}

	// The happy path: three statements, no callback, no error.
	ops := []runtime.BatchOp{
		ins(1, 10),
		ins(2, 20),
		{SQL: `UPDATE ` + ns + `.nb SET n = n + 1 WHERE id = $1`, Args: []any{1}},
	}
	if err := ex.Batch(ctx, ops, nil); err != nil {
		t.Fatalf("a batch with no callback: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT n FROM `+ns+`.nb WHERE id = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Errorf("n = %d, want 11 — the batch did not apply", n)
	}

	// And an error still surfaces rather than being swallowed with the
	// callback that used to report it.
	err = ex.Batch(ctx, []runtime.BatchOp{ins(3, 30), ins(1, 99)}, nil)
	if err == nil {
		t.Fatal("a duplicate key in a batch with no callback was swallowed")
	}
	if !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("err = %v, want a classified unique violation", err)
	}
}
