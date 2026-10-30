package planspike_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
)

// textMatchCases is the one subject every back end's literal-match lowering is
// asked about. Each wildcard LIKE knows, and the escape character itself,
// appears in it, so a value that is read as a pattern anywhere gets a
// different answer from one that is read as text.
const textMatchSubject = "50% off_[x]!"

var textMatchCases = []struct {
	op, v string
	want  bool
}{
	{"TextHasPrefix", "50%", true},
	{"TextHasPrefix", "", true},
	{"TextHasPrefix", "5_", false}, // `_` as a wildcard would match "50"
	{"TextHasPrefix", "%", false},  // `%` as a wildcard would match anything
	{"TextHasSuffix", "[x]!", true},
	{"TextHasSuffix", "!", true},
	{"TextHasSuffix", "_", false},
	{"TextContains", "% off_", true},
	{"TextContains", "[x]", true},
	{"TextContains", "%%", false},
	{"TextContains", "!!", false}, // the escape character is escaped too
	{"TextContains", "off%", false},
}

// The lowering, evaluated by the server against a literal subject: the value
// is text, never a pattern.
func TestTextMatch_ValueIsLiteral(t *testing.T) {
	ctx := context.Background()
	subject := "'" + textMatchSubject + "'"
	for _, c := range textMatchCases {
		a, b, ok := pgsql.Frag(c.op, subject)
		if !ok {
			t.Fatalf("%s has no lowering", c.op)
		}
		var got bool
		if err := pool.QueryRow(ctx, "SELECT "+a+"1"+b, c.v).Scan(&got); err != nil {
			t.Fatalf("%s(%q): %v\n  SELECT %s1%s", c.op, c.v, err, a, b)
		}
		if got != c.want {
			t.Errorf("%s(%q) on %q = %v, want %v", c.op, c.v, textMatchSubject, got, c.want)
		}
	}
}

// Through the generated handles, against stored rows: a search box that sends
// "%" finds the rows that start with a percent sign, not every row.
func TestTextMatch_GeneratedHandles(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	pct := aUser(t, "%lit_prefix@example.com")
	plain := aUser(t, "xlitzprefix@example.com")

	got, err := user.New().Where(user.Email.HasPrefix("%lit_")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != pct.ID {
		t.Fatalf("HasPrefix(%q) matched %d rows, want only %q", "%lit_", len(got), pct.Email)
	}
	got, err = user.New().Where(user.Email.HasSuffix("litzprefix@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != plain.ID {
		t.Fatalf("HasSuffix matched %d rows, want only %q", len(got), plain.Email)
	}
	// `_` must not stand for the z.
	n, err := user.New().EmailContains("lit_prefix").Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Contains(%q) counted %d rows, want 1", "lit_prefix", n)
	}
}

// Each sees exactly what All sees, in the same order, and a row fn keeps is
// still intact after the arena it was decoded into has been retired. The rows
// carry enough text to retire it several times over.
func TestEach_MatchesAllAndKeptRowsSurvive(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	org := anOrg(t)
	for i := 0; i < 40; i++ {
		n := user.Create()
		n.SetEmail(fmt.Sprintf("each-%02d@example.com", i))
		n.SetName("Each")
		n.SetPrefs(runtime.JSON(`{"pad":"` + strings.Repeat(string(rune('a'+i%26)), 4000) + `"}`))
		n.SetStatus("pending")
		n.SetOrgID(org)
		n.SetAge(30)
		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })
	}
	q := user.New().Where(user.Email.HasPrefix("each-")).Order(user.Email.Asc())

	all, err := q.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := 0
	for _, r := range all {
		text += len(r.Email) + len(r.Name) + len(r.Status) + len(r.Prefs)
	}
	if len(all) != 40 || text < 2*runtime.EachSlabCap {
		t.Fatalf("%d rows, %d bytes of text: the arena would never be retired", len(all), text)
	}
	var kept []user.Row
	n := 0
	err = q.Each(ctx, ex, func(r user.Row) error {
		if r.ID != all[n].ID {
			t.Fatalf("row %d: Each read %v, All read %v", n, r.ID, all[n].ID)
		}
		kept = append(kept, r)
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(all) {
		t.Fatalf("Each visited %d rows, All returned %d", n, len(all))
	}
	for i, r := range kept {
		if r.Email != all[i].Email || string(r.Prefs) != string(all[i].Prefs) {
			t.Fatalf("kept row %d changed after its arena was retired: %q, want %q",
				i, r.Email, all[i].Email)
		}
	}
}

// An error from fn stops the read and comes back unwrapped.
func TestEach_StopsOnCallbackError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	stop := errors.New("enough")
	n := 0
	err := user.New().Each(ctx, ex, func(user.Row) error {
		n++
		if n == 3 {
			return stop
		}
		return nil
	})
	if err != stop {
		t.Fatalf("Each returned %v, want the callback's error", err)
	}
	if n != 3 {
		t.Fatalf("fn ran %d times after returning an error at 3", n)
	}
}
