// Package testmodel is the fixture domain used by M1's round-trip tests.
package testmodel

import (
	"net/netip"
	"time"

	"github.com/gsoultan/raorm"
)

// ---- enums ----

type Status string

const (
	StatusPending   Status = "pending"
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

func (Status) EnumValues() []string {
	return []string{"pending", "active", "suspended"}
}

// ---- typed jsonb payload ----

type Prefs struct {
	Theme  string   `json:"theme"`
	Locale string   `json:"locale"`
	Muted  []string `json:"muted,omitempty"`
}

// ---- mixins ----

type SoftDelete struct {
	DeletedAt *time.Time
}

func (sd *SoftDelete) Schema(t *raorm.Table) {
	t.Col(&sd.DeletedAt).Index()
}

type Auditable struct {
	Version int32
}

func (a *Auditable) Schema(t *raorm.Table) {
	t.Col(&a.Version).Default("0").Version()
}

// ---- models ----

type Org struct {
	raorm.Model

	Name string

	Parent   *Org
	Children []Org
	Users    []User
}

func (o *Org) Schema(t *raorm.Table) {
	t.Col(&o.Parent).OnDelete(raorm.Cascade)
	t.Col(&o.Name).Size(200)
	t.Unique(&o.Name)
}

type User struct {
	raorm.Model
	Auditable
	SoftDelete

	Email  string
	Name   string
	Status Status
	Prefs  Prefs
	Scopes []string
	Age    *int16
	LastIP *string

	// Money. numeric, not float: a float64 cannot represent 0.10 and an
	// accounting system that rounds is a defect, not a tolerance.
	Balance raorm.Decimal
	Credit  *raorm.Decimal
	Splits  []raorm.Decimal // numeric[]: exact values, in an array

	Org     Org
	Posts   []Post
	Profile *Profile
}

func (u *User) Schema(t *raorm.Table) {
	// Nullable on purpose: every other array column in this fixture is NOT
	// NULL, so nothing exercised the nil-is-SQL-NULL path that arrays are
	// careful to keep distinct from an empty array.
	t.Col(&u.Splits).Nullable()
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	t.Col(&u.Status).Default("'pending'")
	// Both are NOT NULL and neither is a type generated code can bind yet, so
	// without a default every generated INSERT would fail. The generator says
	// so rather than letting Postgres say it later, in terms of a constraint
	// name instead of a model field.
	t.Col(&u.Prefs).Default("'{}'")
	t.Col(&u.Balance).Numeric(18, 4).Default("0")
	t.Col(&u.Scopes).Default("'{}'")
	t.Col(&u.Org).OnDelete(raorm.Restrict)

	t.Unique(raorm.Lower(&u.Email))
	t.Index(&u.Org, raorm.Desc(&u.CreatedAt))
	t.Index(&u.Scopes).Using(raorm.GIN)
	t.Index(&u.Status).Where("status <> 'suspended'")
	t.Check("age IS NULL OR age BETWEEN 0 AND 150")
}

// Plans is the one reviewable file's worth of load patterns for User — every
// way this system reads a user with its relations, in one place a reviewer can
// count round trips from.
// Projections: the declared column subsets this system reads. Contact is the
// list-endpoint shape; Fin exercises decimal and a nullable through the
// projected scanner.
func (u *User) Projections(p *raorm.Projections) {
	p.Named("Contact", &u.Email, &u.Name)
	p.Named("Fin", &u.Email, &u.Balance, &u.Age)
}

func (u *User) Plans(p *raorm.Plans) {
	// Posts carry their comments: one round trip for the users, one for the
	// posts, one for the comments — three, whatever the row counts.
	p.Named("Feed").
		With(&u.Posts, raorm.Into(func(p *Post) any { return &p.Comments })).
		With(&u.Org)
	p.Named("Summary").With(&u.Org)
}

func (o *Org) Plans(p *raorm.Plans) {
	p.Named("Tree").With(&o.Children).With(&o.Users)
}

type Profile struct {
	raorm.Model

	Bio  *string
	User User
}

func (p *Profile) Schema(t *raorm.Table) {
	t.Col(&p.User).Unique().OnDelete(raorm.Cascade) // unique FK => one-to-one
	t.Col(&p.Bio).Size(2000)
}

type Post struct {
	raorm.Model

	Title       string
	Body        string
	PublishedAt *time.Time

	Author   User
	Comments []Comment
}

func (p *Post) Schema(t *raorm.Table) {
	t.Col(&p.Author).OnDelete(raorm.Cascade)
	t.Col(&p.Title).Size(300)
	t.Index(&p.Author, raorm.NullsLast(raorm.Desc(&p.PublishedAt)))
}

type Comment struct {
	raorm.Model

	Body string

	Post    Post
	Author  User
	Parent  *Comment
	Replies []Comment
}

func (c *Comment) Schema(t *raorm.Table) {
	t.Col(&c.Post).OnDelete(raorm.Cascade)
	t.Col(&c.Author).OnDelete(raorm.Cascade)
	t.Col(&c.Parent).OnDelete(raorm.Cascade)
}

// Booking exercises the exclusion constraint no other Go ORM can express.
// Attachment is the exclusive-arc fixture: a file belonging to exactly one of
// a post, a comment or a user. Three nullable foreign keys and a CHECK, so
// referential integrity survives — the Rails (subject_type, subject_id) pair
// cannot be constrained by any database.
type Attachment struct {
	raorm.Model

	Filename string
	Subject  raorm.OneOf3[Post, Comment, User]
}

func (a *Attachment) Schema(t *raorm.Table) {
	t.Col(&a.Filename).Size(255)
}

// Event exercises the temporal and network types end to end: a calendar date,
// a time of day, an interval with months kept apart from days, inet vs cidr,
// and int8[].
type Event struct {
	raorm.Model

	On     time.Time // a date, not a timestamp — see Schema
	Opens  raorm.TimeOfDay
	Closes *raorm.TimeOfDay
	Window *raorm.Interval
	Addr   netip.Prefix // inet: host bits allowed
	Net    netip.Prefix // cidr: host bits rejected by the database
	Tags   []int64
}

func (e *Event) Schema(t *raorm.Table) {
	t.Col(&e.On).Date()
	t.Col(&e.Opens).Default("'00:00:00'")
	t.Col(&e.Net).Cidr()
	t.Col(&e.Tags).Default("'{}'")
}

type Booking struct {
	raorm.Model

	Room     string
	StartsAt time.Time
	EndsAt   time.Time
	Status   Status
}

func (b *Booking) Schema(t *raorm.Table) {
	t.Col(&b.Room).Size(64)
	// Two bookings for the same room may not overlap in time. `room WITH =`
	// needs the btree_gist extension; the overlap is a range expression because
	// two scalar timestamps cannot express it.
	t.Exclude(
		raorm.With(&b.Room, raorm.OpEq),
		raorm.WithExpr("tstzrange(starts_at, ends_at)", raorm.OpOverlaps),
	).Where("status <> 'suspended'")
}

// All returns every model in the fixture, in registration order.
func All() []any {
	return []any{
		&Org{}, &User{}, &Profile{}, &Post{}, &Comment{}, &Booking{}, &Attachment{}, &Event{},
	}
}
