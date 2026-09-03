package storm_test

// docs/REFERENCE.md, built.
//
// REFERENCE.md documented a modelling API that largely did not exist —
// t.ForeignKey, t.Inverse, t.Plan, t.Set — for the same reason API.md did: it
// was written before the implementation and never reconciled. Prose cannot be
// tested, so the declarations it shows are made here and put through
// storm.Build, which is the same front end `storm generate` runs.
//
// A method that does not exist fails the build; a declaration that the schema
// front end refuses fails the test.

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
)

// docEvery exercises the type-mapping table in REFERENCE.md §1.
type docEvery struct {
	storm.Model

	Flag     bool
	Small    int16
	Medium   int32
	Big      int64
	Ratio32  float32
	Ratio64  float64
	Name     string
	Blob     []byte
	Raw      [16]byte
	At       time.Time
	Clock    storm.TimeOfDay
	Money    storm.Decimal
	Span     storm.Interval
	Window   storm.TstzRange
	Search   storm.TSVector
	Net      netip.Prefix
	Tags     []string
	Labels   map[string]string
	Payload  map[string]any
	Optional *string
}

func (d *docEvery) Schema(t *storm.Table) {
	// §2, on a column.
	t.Col(&d.Name).Size(320).Named("full_name").Comment("the display name")
	t.Col(&d.Money).Numeric(19, 4)
	t.Col(&d.At).Date()
	t.Col(&d.Net).Cidr()
	t.Col(&d.Big).Default("0").Immutable()
	t.Col(&d.Optional).Nullable()
	t.Col(&d.Flag).NotNull()
	t.Col(&d.Search).Generated(storm.RawSQL(`to_tsvector('english', coalesce(name,''))`))
	t.Col(&d.Small).Index()

	// §2, on the table.
	t.Unique(&d.Name, &d.Small)
	t.Index(&d.Medium, storm.Desc(&d.At))
	t.Check(storm.RawSQL(`big >= 0`))
	t.Name("doc_everything")
	t.Comment("every documented column option")
}

// DocAuditable is the optimistic-locking column and the mixin shape in §3.
// Exported, because a mixin must be: reflect cannot reach an unexported
// embedded field, and storm now says so instead of panicking.
type DocAuditable struct {
	Version int32
}

func (a *DocAuditable) Schema(t *storm.Table) {
	t.Col(&a.Version).Default("0").Version()
}

type docOrder struct {
	storm.Model
	DocAuditable

	Total storm.Decimal
}

// §4: foreign key, has-many, one-to-one, and the natural key.
type docAuthor struct {
	storm.Model
	Email    string
	Articles []docArticle
}

func (a *docAuthor) Schema(t *storm.Table) { t.Unique(&a.Email) }

type docArticle struct {
	storm.Model
	Title  string
	Author docAuthor
}

func (ar *docArticle) Schema(t *storm.Table) {
	t.Col(&ar.Author).OnDelete(storm.Cascade).OnUpdate(storm.Restrict)
}

type docProfile struct {
	storm.Model
	Author docAuthor
}

func (p *docProfile) Schema(t *storm.Table) {
	t.Col(&p.Author).Unique().OnDelete(storm.Cascade) // unique FK => one-to-one
}

// §4: self-referential many-to-many, and the directed edge.
type docPost struct {
	storm.Model
	Title   string
	Related []docPost
}

// §5: the exclusion constraint.
type docBooking struct {
	storm.Model
	Room   string
	During storm.TstzRange
}

func (b *docBooking) Schema(t *storm.Table) {
	t.Exclude(
		storm.With(&b.Room, storm.OpEq),
		storm.With(&b.During, storm.OpOverlaps),
	)
}

// §6: both polymorphic strategies.
type docAttachment struct {
	storm.Model
	Subject storm.OneOf2[docPost, docArticle]
}

type docAudit struct {
	storm.Model
	Subject storm.AnyRef
}

func (a *docAudit) Schema(t *storm.Table) {
	t.Col(&a.Subject).AcknowledgeNoFK("audit rows outlive their subjects by design")
}

// §1 natural key: a model without storm.Model.
type docSetting struct {
	Key   string
	Value string
}

func (s *docSetting) Schema(t *storm.Table) { t.PrimaryKey(&s.Key) }

// Every declaration REFERENCE.md shows, through the same front end
// `storm generate` runs.
func TestReferenceDocDeclarationsBuild(t *testing.T) {
	_, err := storm.Build(
		&docEvery{}, &docOrder{},
		&docAuthor{}, &docArticle{}, &docProfile{},
		&docPost{}, &docBooking{},
		&docAttachment{}, &docAudit{}, &docSetting{},
	)
	if err != nil {
		t.Fatalf("a declaration REFERENCE.md documents was refused:\n%v", err)
	}
}

// The refusal REFERENCE.md §6 promises: an AnyRef without an acknowledgement.
type docAudit2 struct {
	storm.Model
	Subject storm.AnyRef
}

func TestReferenceDocAnyRefRefusal(t *testing.T) {
	_, err := storm.Build(&docAudit2{})
	if err == nil {
		t.Fatal("storm.AnyRef built with no AcknowledgeNoFK")
	}
	if !strings.Contains(err.Error(), "AcknowledgeNoFK") {
		t.Errorf("the error does not name the fix:\n%v", err)
	}
}
