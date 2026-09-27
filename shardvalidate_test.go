package storm_test

// Shard-key refusals. Every one of these is a model that builds a schema no
// database would ever object to — each shard would hold a perfectly valid
// table — and that returns wrong answers at run time. That is why the check
// lives at build time and why its messages are long: there is no second
// chance to catch any of them.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
)

type shTenant struct {
	storm.Model
	Slug string
}

func (s *shTenant) Schema(t *storm.Table) { t.Col(&s.Slug).Unique().Size(64) }

// --- the key's own shape ---

type shNullableKey struct {
	storm.Model
	TenantID *storm.UUID
}

func (m *shNullableKey) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

func TestShardKeyRefusesANullableColumn(t *testing.T) {
	// NULL names no shard, so a row carrying one could not be written
	// anywhere and could not be found again.
	mustRefuse(t, []any{&shNullableKey{}}, "nullable")
}

type shTimeKey struct {
	storm.Model
	At storm.TimeOfDay
}

func (m *shTimeKey) Schema(t *storm.Table) { t.ShardKey(&m.At) }

func TestShardKeyRefusesATypeItCannotHold(t *testing.T) {
	mustRefuse(t, []any{&shTimeKey{}}, "uuid, text or an integer")
}

type shTwoKeys struct {
	storm.Model
	TenantID storm.UUID
	Region   string
}

func (m *shTwoKeys) Schema(t *storm.Table) {
	t.ShardKey(&m.TenantID)
	t.ShardKey(&m.Region)
}

// Two shard keys are two answers for where a row lives, and a query naming
// only one of them is unfindable under the other.
func TestShardKeyRefusesASecondColumn(t *testing.T) {
	mustRefuse(t, []any{&shTwoKeys{}}, "a row has one home")
}

// Declared, then made nullable in the same Schema method. The check at the
// call site saw a NOT NULL column; the one that counts sees the finished
// schema.
type shNulledAfter struct {
	storm.Model
	TenantID storm.UUID
}

func (m *shNulledAfter) Schema(t *storm.Table) {
	t.ShardKey(&m.TenantID)
	t.Col(&m.TenantID).Nullable()
}

func TestShardKeyIsCheckedAgainstTheFinishedSchema(t *testing.T) {
	mustRefuse(t, []any{&shNulledAfter{}}, "nullable")
}

// --- the relation graph ---

type shShardedOrder struct {
	storm.Model
	TenantID storm.UUID
	Total    storm.Decimal
}

func (m *shShardedOrder) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

// An UNSHARDED table pointing at a sharded one: the read from here carries no
// shard, so the child rows come from whichever database the caller passed.
type shUnshardedReport struct {
	storm.Model
	Order   *shShardedOrder
	Comment string
}

func (m *shUnshardedReport) Schema(t *storm.Table) {}

func TestUnshardedTableMayNotPointAtAShardedOne(t *testing.T) {
	mustRefuse(t, []any{&shUnshardedReport{}, &shShardedOrder{}}, "carries no shard")
}

// Two sharded tables on different keys: the rows can land on different
// databases, and the read finds only the pairs that happened not to.
type shShardedByRegion struct {
	storm.Model
	Region string
	Order  *shShardedOrder
}

func (m *shShardedByRegion) Schema(t *storm.Table) {
	t.Col(&m.Region).Size(16)
	t.ShardKey(&m.Region)
}

func TestTwoShardKeysOnOneRelationAreRefused(t *testing.T) {
	mustRefuse(t, []any{&shShardedByRegion{}, &shShardedOrder{}}, "different databases")
}

// --- what must keep working ---

type shLine struct {
	storm.Model
	TenantID storm.UUID
	Order    *shShardedOrder
	Qty      int32
}

func (m *shLine) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

func TestTwoTablesOnTheSameShardKeyAreFine(t *testing.T) {
	if _, err := storm.Build(&shLine{}, &shShardedOrder{}); err != nil {
		t.Fatalf("co-located tables were refused: %v", err)
	}
}

// A sharded table pointing at an unsharded one is the reference-table case:
// `currencies`, `plans`, `countries` — small, rarely written, copied to every
// shard. The child read runs on the parent's own Bound, so it reads the copy
// that is there.
type shOrderWithCurrency struct {
	storm.Model
	TenantID storm.UUID
	Plan     *shTenant
}

func (m *shOrderWithCurrency) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

func TestShardedTableMayPointAtAReferenceTable(t *testing.T) {
	if _, err := storm.Build(&shOrderWithCurrency{}, &shTenant{}); err != nil {
		t.Fatalf("a reference-table relation was refused: %v", err)
	}
}

// The pass must cost nothing on a schema that does not shard — including not
// refusing relations that were legal before it existed.
func TestUnshardedSchemasAreUntouched(t *testing.T) {
	if _, err := storm.Build(&shTenant{}); err != nil {
		t.Fatalf("an unsharded schema was refused: %v", err)
	}
}

func mustRefuse(t *testing.T, models []any, want string) {
	t.Helper()
	_, err := storm.Build(models...)
	if err == nil {
		t.Fatal("Build accepted it")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not say %q:\n%v", want, err)
	}
}

// --- declared joins ---

type shJoinLine struct {
	storm.Model
	TenantID storm.UUID
	Order    *shShardedOrder
	Qty      int32
}

func (m *shJoinLine) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

func (m *shJoinLine) Joins(j *storm.Joins) {
	var o shShardedOrder
	j.Named("WithOrder").
		Inner(&o, &m.Order).
		Take(&m.Qty, "Qty").
		Take(&o.Total, "Total")
}

// Two tables on the same shard key: the join runs on one database and both
// rows are there.
func TestJoinBetweenCoLocatedTablesIsFine(t *testing.T) {
	if _, err := storm.Build(&shJoinLine{}, &shShardedOrder{}); err != nil {
		t.Fatalf("a co-located join was refused: %v", err)
	}
}

// An unsharded table joining a sharded one: the statement runs on whichever
// database the caller passed and reports one shard's rows as all of them.
type shUnshardedJoiner struct {
	storm.Model
	Order *shShardedOrder
	Note  string
}

func (m *shUnshardedJoiner) Schema(t *storm.Table) {}

func (m *shUnshardedJoiner) Joins(j *storm.Joins) {
	var o shShardedOrder
	j.Named("WithOrder").
		Inner(&o, &m.Order).
		Take(&m.Note, "Note").
		Take(&o.Total, "Total")
}

func TestUnshardedJoinIntoAShardedTableIsRefused(t *testing.T) {
	// It fails the relation rule too; what this pins is that the JOIN is
	// named, because a join reaches tables the relation graph does not.
	mustRefuse(t, []any{&shUnshardedJoiner{}, &shShardedOrder{}}, "join WithOrder")
}

// --- declared unions ---

type shFeedPost struct {
	storm.Model
	TenantID storm.UUID
	Title    string
}

func (m *shFeedPost) Schema(t *storm.Table) { t.ShardKey(&m.TenantID) }

type shFeedEvent struct {
	storm.Model
	Label string
}

func (m *shFeedEvent) Schema(t *storm.Table) {}

var shFeed = storm.Union("Feed", func(u *storm.UnionSpec) {
	var p shFeedPost
	a := u.From(&p)
	a.Take(&p.Title, "Text")
	a.Const("Kind", "post")

	var e shFeedEvent
	b := u.From(&e)
	b.Take(&e.Label, "Text")
	b.Const("Kind", "event")

	u.OrderAsc("Text")
})

// A union has no driving table, so its reader takes a plain Executor and
// there is no Bound to give it instead. One branch reading a sharded table is
// enough: the union would answer for one database as if it were all of them.
func TestUnionOverAShardedTableIsRefused(t *testing.T) {
	_, err := storm.Build(&shFeedPost{}, &shFeedEvent{}, shFeed)
	if err == nil {
		t.Fatal("a union over a sharded table was accepted")
	}
	for _, want := range []string{"union Feed", "no driving table", "shard.Set.Each"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// --- implicit many-to-many ---

type shM2MAuthor struct {
	storm.Model
	OrgID storm.UUID
	Name  string
	Books []shM2MBook
}

func (m *shM2MAuthor) Schema(t *storm.Table) { t.ShardKey(&m.OrgID) }

type shM2MBook struct {
	storm.Model
	OrgID   storm.UUID
	Title   string
	Authors []shM2MAuthor
}

func (m *shM2MBook) Schema(t *storm.Table) { t.ShardKey(&m.OrgID) }

// The join table storm generates holds only the two foreign keys, so there is
// no column to carry a shard key — and its package would take a plain
// Executor, letting a link row be written to any database while both rows it
// joins live on one.
func TestImplicitManyToManyOnShardedTablesIsRefused(t *testing.T) {
	_, err := storm.Build(&shM2MAuthor{}, &shM2MBook{})
	if err == nil {
		t.Fatal("an implicit many-to-many between two sharded tables was accepted")
	}
	for _, want := range []string{"many-to-many", "no column to carry a shard key", "t.Through"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// Both sides unsharded: nothing to do with sharding, must keep working.
type shPlainA struct {
	storm.Model
	Name string
	Bs   []shPlainB
}

func (m *shPlainA) Schema(t *storm.Table) {}

type shPlainB struct {
	storm.Model
	Label string
	As    []shPlainA
}

func (m *shPlainB) Schema(t *storm.Table) {}

func TestImplicitManyToManyWithoutShardingIsFine(t *testing.T) {
	if _, err := storm.Build(&shPlainA{}, &shPlainB{}); err != nil {
		t.Fatalf("an unsharded many-to-many was refused: %v", err)
	}
}
