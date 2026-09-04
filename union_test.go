package storm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
)

type uPost struct {
	storm.Model
	Title  string
	Hidden bool
}

type uEvent struct {
	storm.Model
	Label string
	When  *time.Time
}

// The shape that works, and the two things about it worth asserting: the
// nullability ORs across branches, and the types widen rather than being
// refused for disagreeing harmlessly.
var uGood = storm.Union("Good", func(u *storm.UnionSpec) {
	var p uPost
	a := u.From(&p)
	a.Take(&p.CreatedAt, "At")
	a.Take(&p.Title, "Text")
	a.Const("Kind", "post")
	a.Where(storm.Exprs{}.Eq(&p.Hidden, false))

	var e uEvent
	b := u.From(&e)
	b.Take(&e.When, "At") // *time.Time — nullable
	b.Take(&e.Label, "Text")
	b.Const("Kind", "event")

	u.OrderDesc("At")
})

func TestUnionBuilds(t *testing.T) {
	s, err := storm.Build(&uPost{}, &uEvent{}, uGood)
	if err != nil {
		t.Fatal(err)
	}
	u := s.Union("Good")
	if u == nil {
		t.Fatal("no union built")
	}
	if len(u.Branches) != 2 || len(u.Cols) != 3 {
		t.Fatalf("got %d branches and %d columns, want 2 and 3", len(u.Branches), len(u.Cols))
	}
	// One branch takes a *time.Time, so the merged column is nullable however
	// NOT NULL the other branch's created_at is. Typing it otherwise would
	// decode one branch's NULL as the other's zero time.
	if at := u.Col("At"); at == nil || !at.Nullable {
		t.Errorf("At = %+v, want nullable because one branch can produce NULL", at)
	}
	if u.Distinct {
		t.Error("UNION ALL is the default; nothing asked for dedup")
	}
	if u.Branches[0].Where == nil {
		t.Error("the declared branch filter was dropped")
	}
}

// ---- refusals ---------------------------------------------------------------

func union(fn func(*storm.UnionSpec)) *storm.UnionDecl { return storm.Union("X", fn) }

func TestUnionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		decl *storm.UnionDecl
		want string
	}{
		{"one branch", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			u.OrderDesc("At")
		}), "a union of one"},

		{"different names in the same position", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			a.Take(&p.Title, "Text")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.When, "At")
			b.Take(&e.Label, "Label") // should be "Text"
			u.OrderDesc("At")
		}), "same names in the same order"},

		{"different widths", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			a.Take(&p.Title, "Text")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.When, "At")
			u.OrderDesc("At")
		}), "projects 1 column(s) and the first projects 2"},

		{"types that will not unify", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.Label, "At") // text where the first has timestamptz
			u.OrderDesc("At")
		}), "will not unify"},

		{"no ordering", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.When, "At")
		}), "declares no ordering"},

		{"orders by something not projected", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "At")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.When, "At")
			u.OrderDesc("Nope")
		}), "not one of its output columns"},

		{"a pointer into the wrong model", union(func(u *storm.UnionSpec) {
			var p uPost
			var e uEvent
			a := u.From(&p)
			a.Take(&e.Label, "Text") // e is not this branch's model
			b := u.From(&e)
			b.Take(&e.Label, "Text")
			u.OrderDesc("Text")
		}), "does not point into this branch"},

		{"output name that is not an identifier", union(func(u *storm.UnionSpec) {
			var p uPost
			a := u.From(&p)
			a.Take(&p.CreatedAt, "occurred at")
			var e uEvent
			b := u.From(&e)
			b.Take(&e.When, "occurred at")
			u.OrderDesc("occurred at")
		}), "exported Go identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storm.Build(&uPost{}, &uEvent{}, tc.decl)
			if err == nil {
				t.Fatal("accepted a union PostgreSQL would refuse or that answers wrongly")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not mention %q:\n%v", tc.want, err)
			}
		})
	}
}
