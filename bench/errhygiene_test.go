package bench

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/bench/genuser"
	"github.com/gsoultan/storm/runtime"
)

// Bound values must not reach an error string or a SQL string. Both land
// somewhere less protected than the database — logs, traces, a pasted bug
// report — and a WHERE clause is where the email addresses and the tokens
// are. storm's design already keeps them apart (values travel as parameters,
// errors are static sentinels), which makes this the cheap kind of test:
// it costs nothing and it fails the day someone adds `fmt.Errorf("... %v",
// val)` to be helpful.
const (
	sentinel    = "s3nt1nel-must-never-appear"
	sentinelNum = 918273645
)

// failing stands in for a database that says no, so the terminal's error path
// runs without needing a server that is broken in a particular way.
type failing struct{ err error }

func (f failing) Query(context.Context, string, []any) (runtime.Rows, error) { return nil, f.err }
func (f failing) Exec(context.Context, string, []any) (int64, error)         { return 0, f.err }
func (f failing) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, f.err
}
func (f failing) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return f.err
}

func TestErrorsAndSQLNeverCarryBoundValues(t *testing.T) {
	ctx := context.Background()
	// Every operator kind that takes a value, across text, numeric, uuid and
	// time columns, plus the argless ones for completeness.
	var uuidSentinel [16]byte
	copy(uuidSentinel[:], sentinel)

	queries := map[string]genuser.Query{
		"eq":       genuser.New().EmailEq(sentinel),
		"noteq":    genuser.New().EmailNotEq(sentinel),
		"like":     genuser.New().NameLike("%" + sentinel + "%"),
		"in":       genuser.New().EmailIn(sentinel, sentinel+"-2"),
		"gte":      genuser.New().AgeGte(sentinelNum),
		"lt":       genuser.New().AgeLt(sentinelNum),
		"uuid":     genuser.New().OrgIDEq(uuidSentinel),
		"time":     genuser.New().CreatedAtGte(time.Unix(sentinelNum, 0)),
		"isnull":   genuser.New().AgeIsNull(),
		"composed": genuser.New().EmailEq(sentinel).NameLike(sentinel).AgeGte(sentinelNum).Limit(int64(sentinelNum)),
		"or": genuser.New().Any(
			genuser.Email.Eq(sentinel),
			genuser.Name.Like(sentinel),
		),
		"not":    genuser.New().Not(genuser.Email.Eq(sentinel)),
		"keyset": genuser.New().Order(genuser.Email.Asc()).After(genuser.Row{Email: sentinel}),
	}

	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			// 1. The statement itself. This is the injection property from a
			// different angle: if the value is not in the SQL, it cannot be
			// in a log line that prints the SQL either.
			if sql := q.SQL(); strings.Contains(sql, sentinel) {
				t.Fatalf("the compiled SQL carries the bound value:\n%s", sql)
			}

			// 2. The values ARE in the args — that is where they belong, and
			// asserting it keeps the test honest about what it proved. They
			// arrive as *string, not string: a bound value lives in the
			// binder's arena and the arg points at it, which is how the warm
			// path binds without allocating.
			bd := genuser.GetBinder()
			_, args := q.Prepare(bd)
			found := false
			for _, a := range args {
				if p, ok := a.(*string); ok && p != nil && strings.Contains(*p, sentinel) {
					found = true
				}
			}
			genuser.PutBinder(bd)
			if !found && strings.Contains(name, "eq") {
				t.Fatal("no arg carried the value — the test is not binding what it thinks")
			}

			// 3. Builder-side refusals (Err) name the mistake, never the data.
			if err := q.Err(); err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("Query.Err carries the bound value: %v", err)
			}

			// 4. The terminal error path, with the database refusing.
			boom := failing{err: errBoom}
			if _, err := q.All(ctx, boom, nil); err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("All's error carries the bound value: %v", err)
			}
			if _, _, err := q.One(ctx, boom); err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("One's error carries the bound value: %v", err)
			}
			if _, err := q.Count(ctx, boom); err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("Count's error carries the bound value: %v", err)
			}
			if _, err := q.Exists(ctx, boom); err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("Exists's error carries the bound value: %v", err)
			}
		})
	}
}

// What this test deliberately does NOT claim: that a value can never appear in
// any error a caller sees. PostgreSQL puts the offending value in its own
// diagnostics — `Key (email)=(ada@example.com) already exists` — and that
// message belongs to the server, is what makes a constraint violation
// debuggable, and storm must not rewrite it. The property here is narrower and
// enforceable: nothing storm ITSELF writes into a SQL string or an error
// string contains a caller's bound value.
var errBoom = boomError{}

type boomError struct{}

func (boomError) Error() string { return "storm test: the database refused" }
