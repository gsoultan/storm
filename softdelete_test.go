package storm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
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
	DeletedAt *time.Time
}

func (m *sdUnique) Schema(t *storm.Table) {
	t.SoftDelete(&m.DeletedAt)
	t.Unique(&m.Email)
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
		t.Fatalf("the documented form was refused:\n%s", e)
	}
}

// A marked row keeps its key. UNIQUE (email) on a soft-delete table therefore
// promises the address can never be reused — by anyone, including the person
// coming back. Nothing fails until somebody re-registers, in production.
func TestSoftDelete_PlainUniqueIsRefusedWithTheFix(t *testing.T) {
	e := buildErr(t, &sdUnique{})
	if e == "" {
		t.Fatal("UNIQUE on a soft-delete table was accepted")
	}
	for _, want := range []string{
		"can never be used again",
		"PARTIAL INDEX",
		".Unique().Where(",
		"deleted_at IS NULL",
	} {
		if !strings.Contains(e, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, e)
		}
	}
	// A refusal that does not say what to write instead is a wall.
	if !strings.Contains(e, "t.Index(&m.Email)") {
		t.Errorf("the refusal does not name the replacement declaration:\n%s", e)
	}
}

// NULL is the row that is alive, so the column has to be able to hold it.
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
