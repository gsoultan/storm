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
	"github.com/gsoultan/storm/compile/pgddl"
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

// ---- docs/COMPLEX-QUERIES.md ------------------------------------------------
//
// The eight scenarios use hypothetical models, but the DECLARATIONS have to be
// real — that page exists to say where the line is, and it is worth nothing if
// the examples on the declarable side do not build.

type cqPlan struct {
	storm.Model
	Name string
}

type cqSubscription struct {
	storm.Model
	Plan         cqPlan
	Status       string
	MonthlyCents int64
	StartedAt    time.Time
}

// Scenario 1: MRR by plan, month over month.
func (s *cqSubscription) Aggregates(a *storm.Aggregates) {
	m := a.Named("MRR")
	month := m.ByExpr("Month", a.DateTrunc("month", &s.StartedAt))
	m.By(&s.Plan)

	mrr := m.Sum(&s.MonthlyCents, "MRRCents")
	m.Count("Active").Filter(a.Eq(&s.Status, "active"))
	m.Count("Trialing").Filter(a.Eq(&s.Status, "trialing"))
	m.Lag(mrr, "PrevCents", a.Over().PartitionBy(&s.Plan).OrderByAsc(month))
	m.SumOver(mrr, "MonthTotal", a.Over().PartitionBy(month).
		Rows(a.UnboundedPreceding(), a.UnboundedFollowing()))
}

func (s *cqSubscription) Joins(j *storm.Joins) {
	var p cqPlan
	j.Named("MRRByPlan").
		With("mrr", &cqSubscription{}, "MRR").
		Inner(&p, &s.Plan).
		LeftWith("mrr", j.OnCols("mrr", "plan_id", &p.ID)).
		Take(&p.Name, "PlanName").
		TakeFrom("mrr", "Month", "Month").
		TakeFrom("mrr", "MRRCents", "MRRCents").
		TakeFrom("mrr", "PrevCents", "PrevCents")
}

// Scenario 4: the exclusion constraint that makes double-booking impossible.
type cqBooking struct {
	storm.Model
	Room   int32
	During storm.TstzRange
}

func (b *cqBooking) Schema(t *storm.Table) {
	t.Exclude(
		storm.With(&b.Room, storm.OpEq),
		storm.With(&b.During, storm.OpOverlaps),
	)
}

// Scenario 5: facet counts in one pass.
type cqProduct struct {
	storm.Model
	Category string
	Brand    string
}

func (p *cqProduct) Aggregates(a *storm.Aggregates) {
	f := a.Named("Facets")
	f.By(&p.Category)
	f.By(&p.Brand)
	f.Sets([]string{"Category"}, []string{"Brand"}, nil)
	f.Count("N")
	f.GroupingOf("CategoryIsSubtotal", &p.Category)
}

// COMPLEX-QUERIES.md §9: top N by a measure.
type cqCustomer struct {
	storm.Model
	Name string
}

type cqOrder struct {
	storm.Model
	Customer cqCustomer
	Total    storm.Decimal
}

func (o *cqOrder) Aggregates(a *storm.Aggregates) {
	top := a.Named("TopCustomers")
	top.By(&o.Customer)
	spend := top.Sum(&o.Total, "Spend")
	top.Count("Orders")
	top.OrderDesc(spend)
}

func TestComplexQueriesDocDeclarationsBuild(t *testing.T) {
	_, err := storm.Build(&cqSubscription{}, &cqPlan{}, &cqBooking{}, &cqProduct{},
		&cqOrder{}, &cqCustomer{})
	if err != nil {
		t.Fatalf("a declaration COMPLEX-QUERIES.md documents was refused:\n%v", err)
	}
}

// docs/GETTING-STARTED.md, built: the model the walkthrough declares, plus the
// integer-key variant it shows beside storm.Model.
type gsUser struct {
	storm.Model
	Email string
	Name  string
	Notes []gsNote
}

func (u *gsUser) Schema(t *storm.Table) {
	t.Name("users")
	t.Col(&u.Email).Size(320)
	t.Unique(&u.Email)
}

type gsNote struct {
	storm.Model
	Title    string
	Body     string
	Archived bool
	PinnedAt *time.Time
	User     gsUser
}

func (n *gsNote) Schema(t *storm.Table) {
	t.Name("notes")
	t.Col(&n.User).OnDelete(storm.Cascade)
	t.Col(&n.Archived).Default("false")
}

type gsInvoice struct {
	ID    int64
	Total storm.Decimal
}

func (i *gsInvoice) Schema(t *storm.Table) {
	t.Name("invoices")
	t.PrimaryKey(&i.ID)
	t.Col(&i.ID).Serial()
	t.Col(&i.Total).Numeric(12, 2)
}

func TestGettingStartedDocDeclarationsBuild(t *testing.T) {
	s, err := storm.Build(&gsUser{}, &gsNote{}, &gsInvoice{})
	if err != nil {
		t.Fatalf("a declaration GETTING-STARTED.md documents was refused:\n%v", err)
	}
	ddl := pgddl.Create(s)
	for _, want := range []string{
		`"user_id" uuid NOT NULL`,
		`CONSTRAINT "uq_users_email" UNIQUE ("email")`,
		`REFERENCES "users" ("id") ON DELETE CASCADE`,
		`"archived" bool DEFAULT false NOT NULL`,
		`"pinned_at" timestamptz,`,
		`"id" bigserial`,
		`"total" numeric(12,2)`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("GETTING-STARTED.md's model no longer emits %s:\n%s", want, ddl)
		}
	}
}

// docs/MIGRATING-FROM-GORM.md, built: the draft `storm import` wrote for the
// tables GORM v1.31.2 AutoMigrate created, with the three additions step 2 of
// the guide makes. The claims checked here are the ones the guide rests on:
// that AcrossDeleted keeps GORM's unique index a plain one on a soft-delete
// table, that leaving it off makes the index partial, and that Through over
// GORM's own join table declares a many-to-many without new DDL.
type gormCustomer struct {
	ID        int64
	CreatedAt *time.Time
	UpdatedAt *time.Time
	DeletedAt *time.Time
	Email     string
	Name      string
	Orders    []gormOrder
}

func (m *gormCustomer) Schema(t *storm.Table) {
	t.Name("customers")
	t.PrimaryKey(&m.ID)
	t.Col(&m.ID).Serial()
	t.Col(&m.Email).Size(320)
	t.SoftDelete(&m.DeletedAt)
	t.Col(&m.CreatedAt).Default(storm.Now())
	t.Col(&m.UpdatedAt).Default(storm.Now())
	t.Index(&m.DeletedAt).Named("idx_customers_deleted_at")
	t.Index(&m.Email).Unique().Named("idx_customers_email").AcrossDeleted()
}

type gormOrderTag struct {
	Order gormOrder
	Tag   gormTag
}

func (m *gormOrderTag) Schema(t *storm.Table) {
	t.Name("order_tags")
	t.PrimaryKey(&m.Order, &m.Tag)
	t.Col(&m.Order).ConstraintName("fk_order_tags_order")
	t.Col(&m.Tag).ConstraintName("fk_order_tags_tag")
	t.Col(&m.Tag).NoIndex()
}

type gormOrder struct {
	ID        int64
	CreatedAt *time.Time
	UpdatedAt *time.Time
	Customer  gormCustomer
	Status    string
	Total     int64
	Note      *string
	Tags      []gormTag
}

func (m *gormOrder) Schema(t *storm.Table) {
	t.Name("orders")
	t.PrimaryKey(&m.ID)
	t.Col(&m.ID).Serial()
	t.Through(&m.Tags, &gormOrderTag{})
	t.Col(&m.Customer).ConstraintName("fk_customers_orders")
	t.Col(&m.Status).Size(20).Default("'pending'::character varying")
	t.Index(&m.Customer).Named("idx_orders_customer_id")
	t.Index(&m.Status).Named("idx_orders_status")
}

type gormTag struct {
	ID   int64
	Name string
}

func (m *gormTag) Schema(t *storm.Table) {
	t.Name("tags")
	t.PrimaryKey(&m.ID)
	t.Col(&m.ID).Serial()
	t.Index(&m.Name).Unique().Named("idx_tags_name")
}

// gormCustomerPartial is gormCustomer without AcrossDeleted: the form whose
// unique index the guide says storm rebuilds as a partial one.
type gormCustomerPartial struct {
	ID        int64
	DeletedAt *time.Time
	Email     string
}

func (m *gormCustomerPartial) Schema(t *storm.Table) {
	t.Name("customers")
	t.PrimaryKey(&m.ID)
	t.Col(&m.ID).Serial()
	t.SoftDelete(&m.DeletedAt)
	t.Index(&m.Email).Unique().Named("idx_customers_email")
}

func TestGormMigrationDocDeclarationsBuild(t *testing.T) {
	s, err := storm.Build(&gormCustomer{}, &gormOrderTag{}, &gormOrder{}, &gormTag{})
	if err != nil {
		t.Fatalf("a declaration MIGRATING-FROM-GORM.md documents was refused:\n%v", err)
	}
	ddl := pgddl.Create(s)
	for _, want := range []string{
		`CREATE UNIQUE INDEX "idx_customers_email" ON "customers" ("email");`,
		`"created_at" timestamptz DEFAULT now()`,
		`"id" bigserial`,
		`CONSTRAINT "fk_customers_orders" FOREIGN KEY ("customer_id")`,
		`CONSTRAINT "fk_order_tags_tag" FOREIGN KEY ("tag_id")`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("MIGRATING-FROM-GORM.md's model no longer emits %s:\n%s", want, ddl)
		}
	}
	// Through names GORM's existing join table; it must not add one of its own.
	if n := strings.Count(ddl, "CREATE TABLE"); n != 4 {
		t.Errorf("want 4 tables (Through reuses order_tags), got %d:\n%s", n, ddl)
	}

	p, err := storm.Build(&gormCustomerPartial{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `CREATE UNIQUE INDEX "idx_customers_email" ON "customers" ("email") WHERE deleted_at IS NULL;`; !strings.Contains(pgddl.Create(p), want) {
		t.Errorf("without AcrossDeleted the guide says the index becomes %s; got:\n%s", want, pgddl.Create(p))
	}
}
