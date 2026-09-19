package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// EqLower has to find the row whatever case the caller typed.
func TestEqLower_MatchesRegardlessOfCase(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := aUser(t, "Mixed.Case@Example.COM")

	for _, typed := range []string{
		"mixed.case@example.com",
		"MIXED.CASE@EXAMPLE.COM",
		"Mixed.Case@Example.COM",
	} {
		got, ok, err := user.New().Where(user.Email.EqLower(typed)).One(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("%q found nothing; the row is stored as %q", typed, r.Email)
		}
		if got.ID != r.ID {
			t.Fatalf("%q matched a different row", typed)
		}
	}

	// And it must still be equality: a different address must not match.
	if _, ok, err := user.New().
		Where(user.Email.EqLower("other@example.com")).
		One(ctx, ex); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("an address that was never written matched")
	}
}

// THE POINT of a separate operator. `storm.Lower(&u.Email)` declares a unique
// index on lower(email); the planner reaches it only from a predicate whose
// left side is the SAME expression. Eq compares the bare column and cannot use
// it — which is why the two are different operators rather than one with a
// flag.
func TestEqLower_ReachesTheExpressionIndex(t *testing.T) {
	lowered := plan(t, user.New().Where(user.Email.EqLower("a@example.com")).SQL(), "a@example.com", 1000)
	if !strings.Contains(lowered, "Index Scan") {
		t.Fatalf("EqLower did not reach an index:\n%s", lowered)
	}
	if !strings.Contains(lowered, "lower") {
		t.Fatalf("EqLower reached an index, but not the lower(email) one:\n%s", lowered)
	}

	// The control: the same column and the same value through Eq. If this
	// also used the expression index, the test above would prove nothing.
	bare := plan(t, user.New().Where(user.Email.Eq("a@example.com")).SQL(), "a@example.com", 1000)
	if strings.Contains(bare, "lower") {
		t.Fatalf("Eq reached the lower(email) index, so EqLower proves nothing:\n%s", bare)
	}
}

// plan returns the chosen plan as text. Sequential scans are disabled for the
// probe: on a table this small a seq scan is genuinely cheaper, so leaving the
// choice to cost would test the row count rather than whether the predicate
// can reach the index at all.
func plan(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
