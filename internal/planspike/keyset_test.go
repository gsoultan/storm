package planspike_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// THE GATE for keyset pagination: page a whole table and land on every row
// exactly once. A cursor that is off by one loses a row per page or repeats
// one, and neither shows up in a single-page test.
func TestKeyset_PagesEveryRowExactlyOnce(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	total, err := user.New().Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if total < 100 {
		t.Skipf("need a few pages of rows, have %d", total)
	}

	const page = 137 // deliberately not a divisor of the row count
	seen := make(map[[16]byte]bool, total)
	var last user.Row
	var pages int

	for {
		q := user.New().Order(user.Email.Asc(), user.ID.Asc()).Limit(page)
		if pages > 0 {
			q = q.After(last)
		}
		rows, err := q.All(ctx, ex, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("page %d returned a row already seen: %s", pages, r.Email)
			}
			seen[r.ID] = true
		}
		last = rows[len(rows)-1]
		pages++
		if pages > int(total/page)+2 {
			t.Fatal("paging did not terminate — the cursor is not advancing")
		}
	}

	if int64(len(seen)) != total {
		t.Errorf("paged %d distinct rows, want %d — the cursor skipped some", len(seen), total)
	}
	if pages < 2 {
		t.Errorf("only %d page(s); the test proves nothing", pages)
	}
}

// Descending pagination must page the other way. Reusing `>` here walks away
// from the unseen rows and returns a wrong page rather than an error.
func TestKeyset_FollowsTheOrderingDirection(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	first, err := user.New().Order(user.Email.Desc(), user.ID.Desc()).Limit(10).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := user.New().Order(user.Email.Desc(), user.ID.Desc()).
		After(first[len(first)-1]).Limit(10).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) == 0 {
		t.Fatal("no second page")
	}
	// Descending: every row on page two sorts at or below the last of page one.
	if next[0].Email > first[len(first)-1].Email {
		t.Errorf("descending page two starts at %q, above page one's last %q — the comparison went the wrong way",
			next[0].Email, first[len(first)-1].Email)
	}
	for _, r := range next {
		for _, p := range first {
			if r.ID == p.ID {
				t.Fatal("page two repeats a row from page one")
			}
		}
	}
	if !strings.Contains(user.New().Order(user.Email.Desc()).After(user.Row{}).SQL(), " < ") {
		t.Error("a descending keyset filter did not compile to a < comparison")
	}
}

// It must lower to a row comparison, not an OR-expansion: only the row
// comparison walks a multi-column index once.
func TestKeyset_LowersToARowComparison(t *testing.T) {
	sql := user.New().Order(user.Email.Asc(), user.ID.Asc()).After(user.Row{}).SQL()
	if !strings.Contains(sql, `("email", "id") > (`) {
		t.Errorf("not a row comparison:\n%s", sql)
	}
	if strings.Contains(sql, " OR ") {
		t.Errorf("expanded into ORs, giving up the index walk:\n%s", sql)
	}
}

// The cursor is part of the structure, not the values: paging the same query
// with different cursors must reuse one compiled statement.
func TestKeyset_SharesOneStatementAcrossPages(t *testing.T) {
	a := user.New().Order(user.Email.Asc()).After(user.Row{Email: "a"})
	b := user.New().Order(user.Email.Asc()).After(user.Row{Email: "z"})
	if a.Shape() != b.Shape() {
		t.Error("two pages of one query have different shapes — every page would compile a statement")
	}
	if a.SQL() != b.SQL() {
		t.Error("two pages of one query compiled different SQL")
	}
}

// No single row comparison expresses a mixed ordering, and expanding it into
// ORs would silently give up the index walk. Say so instead.
func TestKeyset_MixedOrderingIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	q := user.New().Order(user.Email.Asc(), user.ID.Desc()).After(user.Row{})
	if q.Err() == nil {
		t.Fatal("a mixed ordering must be an error")
	}
	if _, err := q.All(ctx, ex, nil); err == nil {
		t.Error("the terminal must return the error too")
	} else if !strings.Contains(err.Error(), "same direction") {
		t.Errorf("the error should say what is wrong, got: %v", err)
	}
	_ = errors.Is
}

// After with no explicit Order uses the default ordering, which is the primary
// key — so it still pages correctly rather than silently doing nothing.
func TestKeyset_UsesTheDefaultOrdering(t *testing.T) {
	sql := user.New().After(user.Row{}).SQL()
	if !strings.Contains(sql, `("id") > (`) {
		t.Errorf("After() without Order() did not use the default ordering:\n%s", sql)
	}
}

func TestKeyset_IsZeroAlloc(t *testing.T) {
	r := user.Row{Email: "a@b.com"}
	if n := testing.AllocsPerRun(1000, func() {
		q := user.New().Order(user.Email.Asc(), user.ID.Asc()).After(r).Limit(50)
		if q.Err() != nil {
			t.Fatal(q.Err())
		}
	}); n != 0 {
		t.Errorf("composing a keyset page allocates %v times, want 0", n)
	}
}
