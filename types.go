// Package raorm is the public surface: the types you declare a model with, and
// the builder that turns those models into a schema.
package raorm

import (
	"encoding/hex"
	"github.com/gsoultan/raorm/runtime"
	"time"
)

// UUID keeps the core dependency-free. Map it to your preferred package once
// with a codec rather than importing one here.
type UUID [16]byte

func (u UUID) String() string {
	b := make([]byte, 36)
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b)
}

// Null is the allocation-free nullable. A `*T` in a model becomes a Null[T] in
// the generated row type: a pointer would cost one allocation per non-nil field
// per row, and rows are the hot path.
type Null[T any] struct {
	V     T
	Valid bool
}

func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// Some and None construct a Null[T].
func Some[T any](v T) Null[T] { return Null[T]{V: v, Valid: true} }
func None[T any]() Null[T]    { return Null[T]{} }

// Model is the conventional embedded primary key and timestamps. Embedding it
// is optional — declare your own key if you want a natural one.
//
// It carries its own Schema method, so embedding it is the whole declaration:
// the key gets a default and is immutable, and both timestamps default to now().
type Model struct {
	ID        UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (m *Model) Schema(t *Table) {
	t.Col(&m.ID).Default(GenRandomUUID()).Immutable()
	t.Col(&m.CreatedAt).Default(Now()).Immutable()
	t.Col(&m.UpdatedAt).Default(Now())
}

// Decimal is an exact fixed-point number for a numeric column.
//
// An alias, not a wrapper: the model declares raorm.Decimal and generated code
// reads runtime.Decimal, and those must be the same type or every value would
// need converting at the boundary raorm exists to remove.
//
// float64 is not offered for numeric. It cannot represent 0.10, and an
// accounting system that rounds is a defect rather than a tolerance — so the
// choice is made once, here, instead of by whoever writes the model.
type Decimal = runtime.Decimal

// ParseDecimal reads a decimal from its text form.
func ParseDecimal(s string) (Decimal, error) { return runtime.ParseDecimal(s) }

// Enumer marks a named type as a Postgres enum. Constants are not discoverable
// through reflection, so the type has to list them.
//
//	type Status string
//	const (StatusActive Status = "active"; StatusBanned Status = "banned")
//	func (Status) EnumValues() []string { return []string{"active", "banned"} }
type Enumer interface {
	EnumValues() []string
}

// Schemer is implemented by models that need more than their Go types can say.
//
// The receiver MUST be a pointer. With a value receiver Go copies the struct
// before the method runs, so &u.Email points into the copy and cannot be
// resolved back to a field — the builder rejects that at Build time rather
// than producing a silently wrong schema.
type Schemer interface {
	Schema(t *Table)
}

// Action is a referential action for OnDelete/OnUpdate.
type Action string

const (
	Restrict   Action = "RESTRICT"
	Cascade    Action = "CASCADE"
	SetNull    Action = "SET NULL"
	SetDefault Action = "SET DEFAULT"
	NoAction   Action = "NO ACTION"
)

// Index methods.
const (
	BTree = "btree"
	GIN   = "gin"
	GiST  = "gist"
	Hash  = "hash"
	BRIN  = "brin"
)

// Expr is a raw SQL fragment. It is the one deliberate escape from the typed
// API: conspicuous, reported by `raorm lint --expr`, and validated against the
// database at generate time.
type Expr string

// Now renders the SQL now() default.
func Now() Expr { return "now()" }

// UUIDv7 renders a uuidv7() default. Requires PostgreSQL 18+.
func UUIDv7() Expr { return "uuidv7()" }

// GenRandomUUID renders gen_random_uuid(), built in since PostgreSQL 13. This
// is the default for an embedded raorm.Model because it works everywhere the
// rest of raorm does.
func GenRandomUUID() Expr { return "gen_random_uuid()" }

// Interval is a PostgreSQL interval: months, days and microseconds kept
// separate, because a month has no fixed length and a day is not always 24
// hours. An alias for the same reason Decimal is — the model's type and the
// generated code's type must be one type.
type Interval = runtime.Interval

// TimeOfDay is a PostgreSQL `time` — microseconds since midnight, no date and
// no zone. See runtime.TimeOfDay for why it is not a time.Time.
type TimeOfDay = runtime.TimeOfDay

// MaxTimeOfDay is 24:00:00, which PostgreSQL accepts as a `time`.
const MaxTimeOfDay = runtime.MaxTimeOfDay

// NewTimeOfDay builds a time of day from its parts, reporting false for parts
// out of range rather than normalising them — 25:00 is a mistake, not 01:00
// tomorrow.
func NewTimeOfDay(hour, min, sec, micro int) (TimeOfDay, bool) {
	return runtime.NewTimeOfDay(hour, min, sec, micro)
}
