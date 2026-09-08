package storm_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
)

// The two hazards docs/CONCEPT.md names when it rejects soft delete BY DEFAULT
// are "every query that forgets the predicate" and "unique indexes stop meaning
// what they say". The first is answered by the compiler (see codegen). These
// are the second, and the declarations storm will not accept.

type sdOK struct {
	storm.Model
	Email     string
	DeletedAt *time.Time
}

func (m *sdOK) Schema(t *storm.Table) {
	t.SoftDelete(&m.DeletedAt)
	t.Index(&m.Email).Unique().Where("deleted_at IS NULL")
}

type sdUnique struct {
	storm.Model
	Email     string
	Tenant    string
	ExtRef    string
	DeletedAt *time.Time
}

func (m *sdUnique) Schema(t *storm.Table) {
	t.SoftDelete(&m.DeletedAt)
	t.Unique(&m.Email)
	t.Unique(&m.Tenant, &m.Email)
	t.UniqueAcrossDeleted(&m.ExtRef)
}

type sdNotPointer struct {
	storm.Model
	DeletedAt time.Time
}

func (m *sdNotPointer) Schema(t *storm.Table) { t.SoftDelete(&m.DeletedAt) }

func buildErr(t *testing.T, m ...any) string {
	t.Helper()
	_, err := storm.Build(m...)
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSoftDelete_PartialUniqueIndexIsAccepted(t *testing.T) {
	if e := buildErr(t, &sdOK{}); e != "" {
		t.Fatalf("an explicit partial unique index was refused:\n%s", e)
	}
}

// The requirement: a deleted row and a live row may hold the same value. A
// marked row keeps its key, so an ordinary UNIQUE would mean the value can
// never be used again — and PostgreSQL can only say "unique among live rows"
// as a partial index. So that is what a unique declaration becomes here.
func TestSoftDelete_UniqueIsScopedToLiveRows(t *testing.T) {
	s, err := storm.Build(&sdUnique{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tb := s.Table("sd_uniques")
	if tb == nil {
		t.Fatal("table missing")
	}

	byName := map[string]*schema.Index{}
	for _, ix := range tb.Indexes {
		byName[tb.IndexName(ix)] = ix
	}
	for _, name := range []string{"uq_sd_uniques_email", "uq_sd_uniques_tenant_email"} {
		ix, ok := byName[name]
		if !ok {
			t.Fatalf("%s was not emitted as an index; got %v", name, keysOf(byName))
		}
		if !ix.Unique {
			t.Errorf("%s is not unique", name)
		}
		if ix.Where != "deleted_at IS NULL" {
			t.Errorf("%s is scoped by %q, want the live rows", name, ix.Where)
		}
	}

	// It must no longer be a CONSTRAINT: a constraint cannot carry a predicate,
	// so leaving it there would re-impose uniqueness across deleted rows.
	for _, u := range tb.Uniques {
		if len(u.Columns) > 0 && u.Columns[0] == "email" {
			t.Errorf("email is still a UNIQUE constraint (%v); it would span deleted rows", u.Columns)
		}
	}
}

// The other reading is a real requirement — an identifier that must never be
// reissued — and it stays a constraint over every row.
func TestSoftDelete_UniqueAcrossDeletedStaysAConstraint(t *testing.T) {
	s, err := storm.Build(&sdUnique{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tb := s.Table("sd_uniques")
	found := false
	for _, u := range tb.Uniques {
		if len(u.Columns) == 1 && u.Columns[0] == "ext_ref" {
			found = true
		}
	}
	if !found {
		t.Error("UniqueAcrossDeleted did not survive as a constraint over every row")
	}
	for _, ix := range tb.Indexes {
		if ix.Unique && len(ix.Columns) == 1 && ix.Columns[0].Name == "ext_ref" && ix.Where != "" {
			t.Error("UniqueAcrossDeleted was scoped to live rows anyway")
		}
	}
}

// A table that does not soft-delete keeps ordinary constraints.
type sdPlain struct {
	storm.Model
	Email string
}

func (m *sdPlain) Schema(t *storm.Table) { t.Unique(&m.Email) }

func TestSoftDelete_PlainTableKeepsItsConstraint(t *testing.T) {
	s, err := storm.Build(&sdPlain{})
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Table("sd_plains")
	if len(tb.Uniques) != 1 {
		t.Fatalf("a table without soft delete has %d unique constraint(s), want 1", len(tb.Uniques))
	}
	for _, ix := range tb.Indexes {
		if ix.Where != "" {
			t.Errorf("a table without soft delete gained a partial index: %s", ix.Where)
		}
	}
}

func keysOf(m map[string]*schema.Index) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSoftDelete_NonNullableColumnIsRefused(t *testing.T) {
	e := buildErr(t, &sdNotPointer{})
	if !strings.Contains(e, "*time.Time") {
		t.Fatalf("a non-nullable soft-delete column was not refused clearly:\n%s", e)
	}
}

// The other half of "every read carries the predicate, or the model does not
// build". A declared cross-table read names several tables under aliases, and
// storm does not yet attach the predicate to the right one — so it refuses
// rather than returning rows the application was told are deleted.

type sdOrg struct {
	storm.Model
	Name  string
	Users []sdMember
}

func (o *sdOrg) Schema(t *storm.Table) {}

func (o *sdOrg) Plans(p *storm.Plans) {
	p.Named("WithUsers").With(&o.Users)
}

type sdMember struct {
	storm.Model
	OrgID     storm.UUID
	Org       *sdOrg
	DeletedAt *time.Time
}

func (m *sdMember) Schema(t *storm.Table) { t.SoftDelete(&m.DeletedAt) }

func TestSoftDelete_PlanReadingASoftDeleteTableIsRefused(t *testing.T) {
	e := buildErr(t, &sdOrg{}, &sdMember{})
	if e == "" {
		t.Fatal("a plan that loads a soft-delete table was accepted; it would return deleted rows")
	}
	for _, want := range []string{"WithUsers", "sd_members", "storm.SQL"} {
		if !strings.Contains(e, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, e)
		}
	}
}

// A partial unique index that does not read back is a migration that never
// converges: `storm diff` proposes the same index on every run, and an adopter
// learns to ignore it. Since soft delete now REWRITES a declaration into one of
// these, the round trip is part of the feature rather than a property of the
// index grammar it happens to reuse.
func TestSoftDelete_ScopedUniqueRoundTrips(t *testing.T) {
	c := connect(t)
	ctx := context.Background()

	s, err := storm.Build(&sdUnique{})
	if err != nil {
		t.Fatal(err)
	}
	const ns = "storm_sd_roundtrip"
	got := applyInto(t, c, ns, pgddl.Create(s))

	// What the server stored has to be what the model said.
	var partial int
	for _, tb := range got.Tables {
		for _, ix := range tb.Indexes {
			if ix.Unique && ix.Where != "" {
				partial++
				if !strings.Contains(ix.Where, "deleted_at") {
					t.Errorf("a scoped unique index came back predicated on %q", ix.Where)
				}
			}
		}
	}
	if partial != 2 {
		t.Errorf("%d scoped unique index(es) survived the round trip, want 2", partial)
	}

	if _, err := c.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.For(ctx, c, ns, s)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Fatalf("storm diff is not clean against its own DDL — %d change(s):\n%s",
			len(plan.Changes), plan.SQL())
	}
}
