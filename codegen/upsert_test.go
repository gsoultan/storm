package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A partial unique index is only inferrable WITH its predicate. Without it
// PostgreSQL raises SQLSTATE 42P10 — "there is no unique or exclusion
// constraint matching the ON CONFLICT specification" — at run time, on the
// first row that actually collides, which a test inserting distinct rows
// never reaches.
type upTenant struct {
	storm.Model
	Tenant    string
	Slug      string
	Email     string
	Name      string
	DeletedAt *string
}

func (m *upTenant) Schema(t *storm.Table) {
	t.Index(&m.Tenant, &m.Slug).Unique().Where("deleted_at IS NULL").Named("ix_live_slug")
	t.Unique(storm.Lower(&m.Email))
	t.Unique(&m.Name)
}

func TestUpsert_ConflictSpecs(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{}, &upTenant{}), "uptenant")

	for _, want := range []string{
		// The primary key, a plain unique constraint, an expression unique
		// index, and a partial one — each with the spec PostgreSQL infers from.
		`" ON CONFLICT (\"id\")"`,
		`" ON CONFLICT (\"name\")"`,
		`" ON CONFLICT ((lower(email)))"`,
		`" ON CONFLICT (\"tenant\", \"slug\") WHERE deleted_at IS NULL"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no conflict spec %s", want)
		}
	}
	// And a method per target, named after the keys rather than the index.
	for _, want := range []string{
		"func (n *Ins) OnConflictID() *Ins",
		"func (n *Ins) OnConflictName() *Ins",
		"func (n *Ins) OnConflictLowerEmail() *Ins",
		"func (n *Ins) OnConflictTenantSlug() *Ins",
		"func (n *Ins) DoNothing() *Ins",
		"func (n *Ins) Op() (runtime.BatchOp, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// A plain key column is equal on both sides — that is why the row conflicted —
// so assigning it from EXCLUDED is noise. An EXPRESSION key is not: lower(email)
// matching does not make the emails equal, and the caller's spelling is what
// they meant to store.
func TestUpsert_KeyColumnsAreNotAssignedButExpressionKeysAre(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{}, &upTenant{}), "uptenant")

	// The case index is the position in conflictSpecs, so read the order out
	// of the generated code rather than assuming it.
	specs := between(t, src, "var conflictSpecs = []string{", "\n}")
	body := between(t, src, "func assignable(", "\n}")
	cases := strings.Split(body, "case ")

	for i, spec := range strings.Split(strings.TrimSpace(specs), "\n") {
		if i+1 >= len(cases) {
			t.Fatalf("conflictSpecs has %d entries but assignable has %d cases",
				i+1, len(cases)-1)
		}
		c := cases[i+1]
		for _, col := range []string{"tenant", "slug", "name"} {
			// A plain key column is equal on both sides — that is why the row
			// conflicted — so assigning it from EXCLUDED is noise.
			if strings.Contains(spec, `\"`+col+`\"`) && strings.Contains(c, `"`+col+`"`) {
				t.Errorf("target %d (%s) assigns its own key column %s:\n%s", i, spec, col, c)
			}
		}
		// An EXPRESSION key is different: lower(email) matching does not make
		// the emails equal, and the caller's spelling is what they meant to
		// store. Its column has to stay assignable.
		if strings.Contains(spec, "lower(email)") && !strings.Contains(c, `"email"`) {
			t.Errorf("target %d is keyed on lower(email) but cannot assign email:\n%s", i, c)
		}
	}
}

// between returns the text between two markers, failing the test rather than
// panicking when the generated shape has moved.
func between(t *testing.T, src, open, close string) string {
	t.Helper()
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("generated code has no %q", open)
	}
	rest := src[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("no %q after %q", close, open)
	}
	return rest[:j]
}
