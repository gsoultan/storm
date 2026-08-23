package raorm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/raorm"
)

// Every one of these is a mistake a real user will make. The test asserts the
// message actually says what to do about it — an error nobody can act on is a
// bug with extra steps.

type valRecvModel struct {
	raorm.Model
	Email string
}

// Deliberately a VALUE receiver: Go copies the struct, so &m.Email points into
// the copy and cannot be resolved.
func (m valRecvModel) Schema(t *raorm.Table) { t.Col(&m.Email).Unique() }

func TestBuild_ValueReceiverRejected(t *testing.T) {
	_, err := raorm.Build(&valRecvModel{})
	if err == nil {
		t.Fatal("a value receiver must not silently produce a wrong schema")
	}
	for _, want := range []string{"VALUE receiver", "pointer receiver"} {
		want := want
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q:\n%s", want, err)
		}
	}
}

type foreignPtrModel struct {
	raorm.Model
	Email string
}

var elsewhere struct{ Stray string }

func (m *foreignPtrModel) Schema(t *raorm.Table) { t.Col(&elsewhere.Stray).Unique() }

func TestBuild_ForeignFieldPointerRejected(t *testing.T) {
	_, err := raorm.Build(&foreignPtrModel{})
	if err == nil || !strings.Contains(err.Error(), "does not point into the model") {
		t.Fatalf("pointing at another struct's field must be caught, got: %v", err)
	}
}

type danglingSlice struct {
	raorm.Model
	Others []otherSide // otherSide has no field pointing back
}

type otherSide struct {
	raorm.Model
	Name string
}

func TestBuild_HasManyWithoutInverseRejected(t *testing.T) {
	_, err := raorm.Build(&danglingSlice{}, &otherSide{})
	if err == nil || !strings.Contains(err.Error(), "has-many") {
		t.Fatalf("a has-many with no key on the other side must fail, got: %v", err)
	}
}

type unregisteredRef struct {
	raorm.Model
	Other otherSide
}

func TestBuild_UnregisteredModelRejected(t *testing.T) {
	_, err := raorm.Build(&unregisteredRef{})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("referring to an unregistered model must fail, got: %v", err)
	}
}

type setNullOnRequired struct {
	raorm.Model
	Owner otherSide // value type => NOT NULL
}

func (m *setNullOnRequired) Schema(t *raorm.Table) {
	t.Col(&m.Owner).OnDelete(raorm.SetNull)
}

func TestBuild_SetNullOnRequiredRejected(t *testing.T) {
	_, err := raorm.Build(&setNullOnRequired{}, &otherSide{})
	if err == nil || !strings.Contains(err.Error(), "could never fire") {
		t.Fatalf("SET NULL on a NOT NULL column must fail, got: %v", err)
	}
}

type actionOnNonFK struct {
	raorm.Model
	Email string
}

func (m *actionOnNonFK) Schema(t *raorm.Table) { t.Col(&m.Email).OnDelete(raorm.Cascade) }

func TestBuild_ActionOnNonForeignKeyRejected(t *testing.T) {
	_, err := raorm.Build(&actionOnNonFK{})
	if err == nil || !strings.Contains(err.Error(), "not a foreign key") {
		t.Fatalf("OnDelete on a plain column must fail, got: %v", err)
	}
}

type unsupportedField struct {
	raorm.Model
	Fn func() // no sensible column type
}

func TestBuild_UnsupportedTypeRejected(t *testing.T) {
	_, err := raorm.Build(&unsupportedField{})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("an unmappable field must fail, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Raw(") {
		t.Error("message should point at the Raw escape hatch")
	}
}

type ambiguousA struct {
	raorm.Model
	B *ambiguousB
}
type ambiguousB struct {
	raorm.Model
	A *ambiguousA
}

func TestBuild_AmbiguousOneToOneRejected(t *testing.T) {
	_, err := raorm.Build(&ambiguousA{}, &ambiguousB{})
	if err == nil || !strings.Contains(err.Error(), "equally optional") {
		t.Fatalf("a mutually optional pair has no owner and must fail, got: %v", err)
	}
}

func TestBuild_AllProblemsReportedTogether(t *testing.T) {
	// Two mistakes in one model must produce two lines, not one round trip each.
	_, err := raorm.Build(&multiBad{})
	if err == nil {
		t.Fatal("expected errors")
	}
	if n := strings.Count(err.Error(), "\n"); n < 2 {
		t.Errorf("all problems should be reported together, got:\n%s", err)
	}
	if !strings.Contains(err.Error(), "2 problems") {
		t.Errorf("message should count the problems:\n%s", err)
	}
}

type multiBad struct {
	raorm.Model
	Fn    func()
	Email string
}

func (m *multiBad) Schema(t *raorm.Table) { t.Col(&m.Email).OnDelete(raorm.Cascade) }

// ---- naming and inference ----

type inferShape struct {
	raorm.Model
	HTTPStatus int32
	OrgID      string
	Payload    map[string]any
	Tags       []string
	Blob       []byte
	Optional   *time.Time
	Labels     map[string]string
}

func TestBuild_InfersNamesAndTypes(t *testing.T) {
	s, err := raorm.Build(&inferShape{})
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Table("infer_shapes")
	if tb == nil {
		t.Fatalf("want table infer_shapes, got %v", s.Tables[0].Name)
	}
	for _, c := range []struct{ col, typ string }{
		{"http_status", "int4"},
		{"org_id", "text"},
		{"payload", "jsonb"},
		{"tags", "text[]"},
		{"blob", "bytea"},
		{"optional", "timestamptz"},
		{"labels", "hstore"},
	} {
		got := tb.Column(c.col)
		if got == nil {
			t.Errorf("column %s missing", c.col)
			continue
		}
		if got.Type.SQL() != c.typ {
			t.Errorf("%s: want %s, got %s", c.col, c.typ, got.Type.SQL())
		}
	}
	if tb.Column("optional").NotNull {
		t.Error("a pointer field must be nullable")
	}
	if !tb.Column("tags").NotNull {
		t.Error("a non-pointer slice must be NOT NULL")
	}
}

func TestBuild_Deterministic(t *testing.T) {
	// Byte-identical output across runs is what lets `raorm verify` fail CI on
	// a plain diff.
	var first string
	for i := 0; i < 20; i++ {
		s, err := raorm.Build(&inferShape{}, &otherSide{})
		if err != nil {
			t.Fatal(err)
		}
		got := dump(s)
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d differs from run 0", i)
		}
	}
}

// Plan declaration errors. Each is a mistake that would otherwise surface as a
// generated type that does not compile, or worse, one that does.
func TestPlans_DeclarationErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		want  string
	}{
		{"scalar field is not a relation", &planScalar{}, "not a relation"},
		{"duplicate plan name", &planDupName{}, "declared twice"},
		{"same relation twice in one plan", &planDupRel{}, "twice"},
		{"plan name is not an exported identifier", &planBadName{}, "exported Go identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := raorm.Build(tc.model, &planTarget{})
			if err == nil {
				t.Fatal("expected a build error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

type planTarget struct {
	raorm.Model
	Name string
}

type planScalar struct {
	raorm.Model
	Name    string
	Targets []planTarget
}

func (p *planScalar) Plans(pl *raorm.Plans) { pl.Named("Bad").With(&p.Name) }

type planDupName struct {
	raorm.Model
	Targets []planTarget
}

func (p *planDupName) Plans(pl *raorm.Plans) {
	pl.Named("Feed").With(&p.Targets)
	pl.Named("Feed").With(&p.Targets)
}

type planDupRel struct {
	raorm.Model
	Targets []planTarget
}

func (p *planDupRel) Plans(pl *raorm.Plans) {
	pl.Named("Feed").With(&p.Targets).With(&p.Targets)
}

type planBadName struct {
	raorm.Model
	Targets []planTarget
}

func (p *planBadName) Plans(pl *raorm.Plans) { pl.Named("feed").With(&p.Targets) }

// A value receiver copies the struct, so the field pointer points into the copy
// and the offset is garbage. Same trap as Schema, and it must be caught the
// same way rather than producing a plan that silently names the wrong field.
func TestPlans_ValueReceiverIsAnError(t *testing.T) {
	_, err := raorm.Build(&planValueRecv{}, &planTarget{})
	if err == nil {
		t.Fatal("a value receiver must be an error")
	}
	if !strings.Contains(err.Error(), "POINTER receiver") {
		t.Errorf("the error should name the fix, got: %v", err)
	}
}

type planValueRecv struct {
	raorm.Model
	Targets []planTarget
}

func (p planValueRecv) Plans(pl *raorm.Plans) { pl.Named("Feed").With(&p.Targets) }
