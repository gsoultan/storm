package storm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
)

// Every one of these is a mistake a real user will make. The test asserts the
// message actually says what to do about it — an error nobody can act on is a
// bug with extra steps.

type valRecvModel struct {
	storm.Model
	Email string
}

// Deliberately a VALUE receiver: Go copies the struct, so &m.Email points into
// the copy and cannot be resolved.
func (m valRecvModel) Schema(t *storm.Table) { t.Col(&m.Email).Unique() }

func TestBuild_ValueReceiverRejected(t *testing.T) {
	_, err := storm.Build(&valRecvModel{})
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
	storm.Model
	Email string
}

var elsewhere struct{ Stray string }

func (m *foreignPtrModel) Schema(t *storm.Table) { t.Col(&elsewhere.Stray).Unique() }

func TestBuild_ForeignFieldPointerRejected(t *testing.T) {
	_, err := storm.Build(&foreignPtrModel{})
	if err == nil || !strings.Contains(err.Error(), "does not point into the model") {
		t.Fatalf("pointing at another struct's field must be caught, got: %v", err)
	}
}

type danglingSlice struct {
	storm.Model
	Others []otherSide // otherSide has no field pointing back
}

type otherSide struct {
	storm.Model
	Name string
}

func TestBuild_HasManyWithoutInverseRejected(t *testing.T) {
	_, err := storm.Build(&danglingSlice{}, &otherSide{})
	if err == nil || !strings.Contains(err.Error(), "has-many") {
		t.Fatalf("a has-many with no key on the other side must fail, got: %v", err)
	}
}

type unregisteredRef struct {
	storm.Model
	Other otherSide
}

func TestBuild_UnregisteredModelRejected(t *testing.T) {
	_, err := storm.Build(&unregisteredRef{})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("referring to an unregistered model must fail, got: %v", err)
	}
}

type setNullOnRequired struct {
	storm.Model
	Owner otherSide // value type => NOT NULL
}

func (m *setNullOnRequired) Schema(t *storm.Table) {
	t.Col(&m.Owner).OnDelete(storm.SetNull)
}

func TestBuild_SetNullOnRequiredRejected(t *testing.T) {
	_, err := storm.Build(&setNullOnRequired{}, &otherSide{})
	if err == nil || !strings.Contains(err.Error(), "could never fire") {
		t.Fatalf("SET NULL on a NOT NULL column must fail, got: %v", err)
	}
}

type actionOnNonFK struct {
	storm.Model
	Email string
}

func (m *actionOnNonFK) Schema(t *storm.Table) { t.Col(&m.Email).OnDelete(storm.Cascade) }

func TestBuild_ActionOnNonForeignKeyRejected(t *testing.T) {
	_, err := storm.Build(&actionOnNonFK{})
	if err == nil || !strings.Contains(err.Error(), "not a foreign key") {
		t.Fatalf("OnDelete on a plain column must fail, got: %v", err)
	}
}

type unsupportedField struct {
	storm.Model
	Fn func() // no sensible column type
}

func TestBuild_UnsupportedTypeRejected(t *testing.T) {
	_, err := storm.Build(&unsupportedField{})
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("an unmappable field must fail, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Raw(") {
		t.Error("message should point at the Raw escape hatch")
	}
}

type ambiguousA struct {
	storm.Model
	B *ambiguousB
}
type ambiguousB struct {
	storm.Model
	A *ambiguousA
}

func TestBuild_AmbiguousOneToOneRejected(t *testing.T) {
	_, err := storm.Build(&ambiguousA{}, &ambiguousB{})
	if err == nil || !strings.Contains(err.Error(), "equally optional") {
		t.Fatalf("a mutually optional pair has no owner and must fail, got: %v", err)
	}
}

func TestBuild_AllProblemsReportedTogether(t *testing.T) {
	// Two mistakes in one model must produce two lines, not one round trip each.
	_, err := storm.Build(&multiBad{})
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
	storm.Model
	Fn    func()
	Email string
}

func (m *multiBad) Schema(t *storm.Table) { t.Col(&m.Email).OnDelete(storm.Cascade) }

// ---- naming and inference ----

type inferShape struct {
	storm.Model
	HTTPStatus int32
	OrgID      string
	Payload    map[string]any
	Tags       []string
	Blob       []byte
	Optional   *time.Time
	Labels     map[string]string
}

func TestBuild_InfersNamesAndTypes(t *testing.T) {
	s, err := storm.Build(&inferShape{})
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
	// Byte-identical output across runs is what lets `storm verify` fail CI on
	// a plain diff.
	var first string
	for i := 0; i < 20; i++ {
		s, err := storm.Build(&inferShape{}, &otherSide{})
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
			_, err := storm.Build(tc.model, &planTarget{})
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
	storm.Model
	Name string
}

type planScalar struct {
	storm.Model
	Name    string
	Targets []planTarget
}

func (p *planScalar) Plans(pl *storm.Plans) { pl.Named("Bad").With(&p.Name) }

type planDupName struct {
	storm.Model
	Targets []planTarget
}

func (p *planDupName) Plans(pl *storm.Plans) {
	pl.Named("Feed").With(&p.Targets)
	pl.Named("Feed").With(&p.Targets)
}

type planDupRel struct {
	storm.Model
	Targets []planTarget
}

func (p *planDupRel) Plans(pl *storm.Plans) {
	pl.Named("Feed").With(&p.Targets).With(&p.Targets)
}

type planBadName struct {
	storm.Model
	Targets []planTarget
}

func (p *planBadName) Plans(pl *storm.Plans) { pl.Named("feed").With(&p.Targets) }

// A value receiver copies the struct, so the field pointer points into the copy
// and the offset is garbage. Same trap as Schema, and it must be caught the
// same way rather than producing a plan that silently names the wrong field.
func TestPlans_ValueReceiverIsAnError(t *testing.T) {
	_, err := storm.Build(&planValueRecv{}, &planTarget{})
	if err == nil {
		t.Fatal("a value receiver must be an error")
	}
	if !strings.Contains(err.Error(), "POINTER receiver") {
		t.Errorf("the error should name the fix, got: %v", err)
	}
}

type planValueRecv struct {
	storm.Model
	Targets []planTarget
}

func (p planValueRecv) Plans(pl *storm.Plans) { pl.Named("Feed").With(&p.Targets) }

// Projection declaration errors, each caught at build time with the mistake
// named — the alternative is a generated type that is silently wrong.
func TestProjections_DeclarationErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		want  string
	}{
		{"duplicate name", &projDupName{}, "declared twice"},
		{"duplicate column", &projDupCol{}, "twice"},
		{"empty", &projEmpty{}, "no columns"},
		{"reserved name", &projReserved{}, "reserved"},
		{"unexported name", &projBadName{}, "exported Go identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storm.Build(tc.model)
			if err == nil {
				t.Fatal("expected a build error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

type projDupName struct {
	storm.Model
	A string
}

func (m *projDupName) Projections(p *storm.Projections) {
	p.Named("X", &m.A)
	p.Named("X", &m.A)
}

type projDupCol struct {
	storm.Model
	A string
}

func (m *projDupCol) Projections(p *storm.Projections) { p.Named("X", &m.A, &m.A) }

type projEmpty struct {
	storm.Model
	A string
}

func (m *projEmpty) Projections(p *storm.Projections) { p.Named("X") }

type projReserved struct {
	storm.Model
	A string
}

func (m *projReserved) Projections(p *storm.Projections) { p.Named("Into", &m.A) }

type projBadName struct {
	storm.Model
	A string
}

func (m *projBadName) Projections(p *storm.Projections) { p.Named("contact", &m.A) }

// An unexported mixin with a Schema method used to PANIC inside reflect:
// callMixinSchemas called Interface() on an embedded field it could not read,
// while walk right below it skipped unexported fields correctly.
//
// Skipping silently would have been worse than the panic. The mixin's Schema
// never runs, so the table comes out missing whatever it declared — a default,
// a version column — and nothing says so.
type unexportedMixin struct {
	Version int32
}

func (m *unexportedMixin) Schema(t *storm.Table) {
	t.Col(&m.Version).Default("0").Version()
}

type embedsUnexported struct {
	storm.Model
	unexportedMixin

	Name string
}

func TestUnexportedMixinIsAnErrorNotAPanic(t *testing.T) {
	_, err := storm.Build(&embedsUnexported{})
	if err == nil {
		t.Fatal("an unexported mixin built cleanly; its Schema cannot have run")
	}
	for _, want := range []string{"unexported", "Schema", "export"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}
