package bench

import (
	"context"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gsoultan/raorm/bench/genuser"
	"github.com/gsoultan/raorm/internal/spike"
	"github.com/gsoultan/raorm/runtime"
	"github.com/gsoultan/raorm/runtime/pgxdrv"
)

// M2's kill criterion: generated code must reproduce the M0 spike within 5%.
// If a generator cannot emit what a human wrote by hand, every later milestone
// inherits slow code.

func genExec() runtime.Executor { return pgxdrv.Pool{P: pool} }

func genDynamic6() genuser.Query {
	return genuser.New().
		OrgIDEq(orgs[3]).
		EmailEq("user000003@corp.com").
		NameLike("User %").
		AgeGte(21).
		StatusEq("active").
		CreatedAtGte(time.Now().Add(-400 * 24 * time.Hour)).
		Limit(50)
}

// TestGenMatchesSpikeSQL proves the two paths compile the same statement, so
// the benchmark below compares implementations and not query plans.
func TestGenMatchesSpikeSQL(t *testing.T) {
	// The generator quotes every identifier and the spike does not. That is a
	// difference in the generator's favour — quoting is what makes a column
	// named "order" or "user" work — so compare with quotes removed.
	got := strings.ReplaceAll(genDynamic6().SQL(), `"`, "")
	want := dynamic6().SQL()
	if got != want {
		t.Fatalf("generated and hand-written SQL differ:\n gen:   %s\n spike: %s", got, want)
	}
}

func TestGenMatchesSpikeRows(t *testing.T) {
	ctx := context.Background()
	q := genuser.New().StatusEq("active").Limit(200)
	got, err := q.All(ctx, genExec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := spike.New().Status("active").Limit(200).All(ctx, spike.PgxExec{Pool: pool}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("generated %d rows, spike %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Email != w.Email || g.Name != w.Name || g.Status != w.Status ||
			!g.CreatedAt.Equal(w.CreatedAt) || g.Age.Valid != w.Age.Valid || g.Age.V != w.Age.V {
			t.Fatalf("row %d differs:\n gen   %+v\n spike %+v", i, g, w)
		}
	}
}

// ---- the gate ----

// Same work as BenchmarkPrepare_Warm on the spike: resolve the shape AND bind
// arguments. Comparing a bare cache lookup against lookup-plus-bind would
// flatter the generator for no reason.
func BenchmarkGenPrepare_Warm(b *testing.B) {
	q := genDynamic6()
	q.SQL()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bd := genuser.GetBinder()
		sql, args := q.Prepare(bd)
		if len(sql) == 0 || len(args) != 7 {
			b.Fatal("bad prepare", len(args))
		}
		genuser.PutBinder(bd)
	}
}

func BenchmarkGenBuildAndPrepare_Warm(b *testing.B) {
	genDynamic6().SQL()
	now := time.Now().Add(-400 * 24 * time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := genuser.New().
			OrgIDEq(orgs[3]).EmailEq("user000003@corp.com").NameLike("User %").
			AgeGte(21).StatusEq("active").CreatedAtGte(now).Limit(50)
		bd := genuser.GetBinder()
		_, args := q.Prepare(bd)
		if len(args) != 7 {
			b.Fatal("bad prepare")
		}
		genuser.PutBinder(bd)
	}
}

func BenchmarkGenScan1000(b *testing.B) {
	ctx, ex := context.Background(), genExec()
	q := genuser.New().StatusEq("active").Limit(1000)
	buf := make([]genuser.Row, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl runtime.Slab
		var err error
		if buf, err = q.AllInto(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
		if len(buf) != 1000 {
			b.Fatal(len(buf))
		}
	}
}

func BenchmarkGenDynamic6(b *testing.B) {
	ctx, ex := context.Background(), genExec()
	q := genDynamic6()
	buf := make([]genuser.Row, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl runtime.Slab
		var err error
		if buf, err = q.AllInto(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- composable predicates ----

// Reusable specifications: the composability argument for objects over
// strings. Each of these can be returned from a function, stored, and shared
// between queries, and a column rename breaks every use at compile time.
var (
	genActive = genuser.Status.Eq("active")
	genAdult  = genuser.Age.Gte(21)
)

func genInOrg(id [16]byte) genuser.Pred { return genuser.OrgID.Eq(id) }

func TestGenComposablePredsMatchChained(t *testing.T) {
	chained := genuser.New().
		OrgIDEq(orgs[3]).StatusEq("active").AgeGte(21).Limit(50)
	composed := genuser.New().
		Where(genInOrg(orgs[3]), genActive, genAdult).Limit(50)

	if chained.Shape() != composed.Shape() {
		t.Fatalf("shape differs: chained %d, composed %d", chained.Shape(), composed.Shape())
	}
	if chained.SQL() != composed.SQL() {
		t.Fatalf("SQL differs:\n chained:  %s\n composed: %s", chained.SQL(), composed.SQL())
	}

	ctx, ex := context.Background(), genExec()
	a, err := chained.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := composed.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("row counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("row %d differs", i)
		}
	}
}

func TestGenWhereIf(t *testing.T) {
	base := genuser.New().Where(genActive)
	if got := base.WhereIf(false, genAdult); got.Shape() != base.Shape() {
		t.Error("WhereIf(false) must not change the shape")
	}
	if got := base.WhereIf(true, genAdult); got.Shape() == base.Shape() {
		t.Error("WhereIf(true) must change the shape")
	}
}

func BenchmarkGenWhere_Composed(b *testing.B) {
	genuser.New().Where(genActive, genAdult).SQL()
	id := orgs[3]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := genuser.New().Where(genInOrg(id), genActive, genAdult).Limit(50)
		bd := genuser.GetBinder()
		_, args := q.Prepare(bd)
		if len(args) != 4 {
			b.Fatal(len(args))
		}
		genuser.PutBinder(bd)
	}
}

func BenchmarkGenWhere_Chained(b *testing.B) {
	genuser.New().OrgIDEq(orgs[3]).StatusEq("active").AgeGte(21).SQL()
	id := orgs[3]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := genuser.New().OrgIDEq(id).StatusEq("active").AgeGte(21).Limit(50)
		bd := genuser.GetBinder()
		_, args := q.Prepare(bd)
		if len(args) != 4 {
			b.Fatal(len(args))
		}
		genuser.PutBinder(bd)
	}
}

func TestGenCountAndExists(t *testing.T) {
	ctx, ex := context.Background(), genExec()

	n, err := genuser.New().Where(genActive).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	// A Count must ignore the LIMIT: counting a truncated set is a bug.
	nLimited, err := genuser.New().Where(genActive).Limit(10).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != nLimited {
		t.Errorf("Count must ignore Limit: %d vs %d", n, nLimited)
	}
	if n < 1000 {
		t.Errorf("expected many active users, got %d", n)
	}

	// Cross-check against the row count.
	rows, err := genuser.New().Where(genActive).Limit(100000).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rows)) != n {
		t.Errorf("Count says %d, All returned %d", n, len(rows))
	}

	ok, err := genuser.New().Where(genuser.Email.Eq("user000003@corp.com")).Exists(ctx, ex)
	if err != nil || !ok {
		t.Errorf("Exists on a known row: %v %v", ok, err)
	}
	ok, err = genuser.New().Where(genuser.Email.Eq("nobody@nowhere")).Exists(ctx, ex)
	if err != nil || ok {
		t.Errorf("Exists on a missing row: %v %v", ok, err)
	}
}

// ---- relation loading: the N+1 guarantee ----

// TestAnyBindsOneStatement is the property everything else rests on: a list of
// any length binds to ONE placeholder, so the statement text does not change
// with the number of ids. Without it, loading 50 children would mint a new
// statement per distinct list length.
func TestAnyBindsOneStatement(t *testing.T) {
	a := genuser.New().IDIn(ids[0])
	b := genuser.New().IDIn(ids[0:50]...)
	c := genuser.New().IDIn(ids[0:500]...)
	if a.SQL() != b.SQL() || b.SQL() != c.SQL() {
		t.Fatalf("list length must not change the statement:\n 1:   %s\n 50:  %s\n 500: %s",
			a.SQL(), b.SQL(), c.SQL())
	}
	if !strings.Contains(a.SQL(), "= ANY($") {
		t.Errorf("want = ANY, got %s", a.SQL())
	}
	if a.Shape() != c.Shape() {
		t.Error("list length must not change the shape either")
	}
}

func TestAnyReturnsExactly(t *testing.T) {
	ctx, ex := context.Background(), genExec()
	want := ids[10:60]
	got, err := genuser.New().IDIn(want...).Limit(1000).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("= ANY returned %d rows, want %d", len(got), len(want))
	}
	seen := map[[16]byte]bool{}
	for _, r := range got {
		seen[r.ID] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("missing id %x", id[:4])
		}
	}
}

// TestRelationLoadIsTwoRoundTrips is the N+1 guarantee, asserted rather than
// hoped for: loading N parents and all their children costs exactly two
// statements, whatever N is.
func TestRelationLoadIsTwoRoundTrips(t *testing.T) {
	ctx := context.Background()
	counter := &runtime.CountingExecutor{Inner: genExec()}

	// 1. the parents
	parents, err := genuser.New().Where(genActive).Limit(50).All(ctx, counter, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 50 {
		t.Fatalf("want 50 parents, got %d", len(parents))
	}

	// 2. every child, in one statement, keyed by the parents we just read
	orgIDs := make([][16]byte, 0, len(parents))
	seen := map[[16]byte]bool{}
	for _, p := range parents {
		if !seen[p.OrgID] {
			seen[p.OrgID] = true
			orgIDs = append(orgIDs, p.OrgID)
		}
	}
	children, err := genuser.New().OrgIDIn(orgIDs...).Limit(100000).All(ctx, counter, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) == 0 {
		t.Fatal("no children loaded")
	}

	if n := counter.RoundTrips(); n != 2 {
		t.Fatalf("relation load took %d round trips, want exactly 2 — this is the N+1 guarantee", n)
	}
	t.Logf("50 parents + %d children across %d distinct orgs in %d round trips",
		len(children), len(orgIDs), counter.RoundTrips())
}

func BenchmarkGenAny500(b *testing.B) {
	ctx, ex := context.Background(), genExec()
	q := genuser.New().IDIn(ids[0:500]...).Limit(1000)
	buf := make([]genuser.Row, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl runtime.Slab
		var err error
		if buf, err = q.AllInto(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
		if len(buf) != 500 {
			b.Fatal(len(buf))
		}
	}
}

// ---- disjunction and negation: impossible under the old per-column mask ----

func TestOrAndNot(t *testing.T) {
	ctx, ex := context.Background(), genExec()

	// A AND (B OR C) — one column used twice, and a nested group. Neither is
	// representable as four bits of operator per column.
	q := genuser.New().
		Where(genuser.Status.Eq("active")).
		Any(genuser.Age.Lt(25), genuser.Age.Gt(70)).
		Limit(5000)

	sql := q.SQL()
	if !strings.Contains(sql, " OR ") {
		t.Fatalf("want a disjunction, got %s", sql)
	}
	rows, err := q.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		age, ok := r.Age.Get()
		if r.Status != "active" || !ok || (age >= 25 && age <= 70) {
			t.Fatalf("row violates status='active' AND (age<25 OR age>70): %+v", r)
		}
	}
	t.Logf("%d rows: %s", len(rows), sql)
}

func TestNot(t *testing.T) {
	ctx, ex := context.Background(), genExec()
	q := genuser.New().
		Where(genuser.OrgID.Eq(orgs[3])).
		Not(genuser.Status.Eq("active")).
		Limit(5000)

	if !strings.Contains(q.SQL(), "NOT (") {
		t.Fatalf("want a negation, got %s", q.SQL())
	}
	rows, err := q.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if r.Status == "active" || r.OrgID != orgs[3] {
			t.Fatalf("row violates org=3 AND NOT status='active': %+v", r)
		}
	}
}

// TestOrMatchesPostgres cross-checks the compiled SQL against the database's
// own answer, so a mis-parenthesised tree cannot pass. AND binds tighter than
// OR, so a missing group would silently return the wrong rows.
func TestOrPrecedence(t *testing.T) {
	ctx, ex := context.Background(), genExec()

	got, err := genuser.New().
		Where(genuser.OrgID.Eq(orgs[3])).
		Any(genuser.Status.Eq("pending"), genuser.Status.Eq("suspended")).
		Limit(10000).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The same question asked directly.
	var want int64
	rows, err := pool.Query(ctx,
		`SELECT count(*) FROM users WHERE org_id = $1 AND (status = $2 OR status = $3)`,
		orgs[3], "pending", "suspended")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		if err := rows.Scan(&want); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()

	if int64(len(got)) != want {
		t.Fatalf("precedence is wrong: raorm returned %d rows, Postgres says %d\n  %s",
			len(got), want, genuser.New().Where(genuser.OrgID.Eq(orgs[3])).
				Any(genuser.Status.Eq("pending"), genuser.Status.Eq("suspended")).SQL())
	}
	if want == 0 {
		t.Fatal("fixture produced no rows; the test proves nothing")
	}
	t.Logf("%d rows agree with Postgres", want)
}

func TestNotAny(t *testing.T) {
	ctx, ex := context.Background(), genExec()
	q := genuser.New().
		NotAny(genuser.Status.Eq("pending"), genuser.Status.Eq("suspended")).
		Limit(5000)
	rows, err := q.All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Status != "active" {
			t.Fatalf("NOT (pending OR suspended) returned %q", r.Status)
		}
	}
}

// Distinct structures must get distinct statements; identical structures with
// different values must share one.
func TestShapeIdentity(t *testing.T) {
	and := genuser.New().Where(genuser.Status.Eq("a"), genuser.Age.Gt(1))
	or := genuser.New().Any(genuser.Status.Eq("a"), genuser.Age.Gt(1))
	if and.Shape() == or.Shape() {
		t.Error("AND and OR of the same leaves must not share a shape")
	}
	if and.SQL() == or.SQL() {
		t.Error("AND and OR must not compile to the same SQL")
	}
	same := genuser.New().Where(genuser.Status.Eq("zzz"), genuser.Age.Gt(999))
	if and.Shape() != same.Shape() {
		t.Error("same structure, different values must share a shape")
	}
	if and.SQL() != same.SQL() {
		t.Error("same structure must compile to the same SQL")
	}
}

func TestQueryOverflowIsAnError(t *testing.T) {
	// Buffers are fixed; outgrowing them must be an error, never a silently
	// dropped predicate.
	q := genuser.New()
	for i := 0; i < 40; i++ {
		q = q.Where(genuser.Status.Eq("active"))
	}
	if q.Err() == nil {
		t.Fatal("outgrowing the buffers must be reported")
	}
	if _, err := q.All(context.Background(), genExec(), nil); err == nil {
		t.Fatal("All must refuse a query that overflowed")
	}
}

// Query is a value type, so each builder call copies it. Chaining six calls
// copies six times; passing six predicates to one Where copies once. If the
// difference is large, that is a usage note, not a defect.
func BenchmarkGenBuild_OneCall(b *testing.B) {
	now := time.Now().Add(-400 * 24 * time.Hour)
	genuser.New().Where(genuser.Status.Eq("active")).SQL()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := genuser.New().Where(
			genuser.OrgID.Eq(orgs[3]),
			genuser.Email.Eq("user000003@corp.com"),
			genuser.Name.Like("User %"),
			genuser.Age.Gte(21),
			genuser.Status.Eq("active"),
			genuser.CreatedAt.Gte(now),
		).Limit(50)
		bd := genuser.GetBinder()
		_, args := q.Prepare(bd)
		if len(args) != 7 {
			b.Fatal(len(args))
		}
		genuser.PutBinder(bd)
	}
}

// A value type's size is part of its API: Query is copied on every builder
// call, and the type-coverage work grew it from ~330 to 704 bytes by emitting
// every arena into every table — a −28% builder regression that no test
// caught, because sizes had no tripwire the way allocations do. These bounds
// are deliberately loose (they allow real growth with a real column); what
// they catch is machinery for kinds this table does not have.
func TestQuerySize_HasATripwire(t *testing.T) {
	if s := unsafe.Sizeof(genuser.Query{}); s > 512 {
		t.Errorf("genuser.Query is %d bytes (was 480 after the diet, 704 at the regression) — "+
			"did an arena become unconditional again?", s)
	}
	if s := unsafe.Sizeof(genuser.Pred{}); s > 136 {
		t.Errorf("genuser.Pred is %d bytes (was 120 after the diet, 176 at the regression)", s)
	}
}

// The projection's whole reason: the same 1,000 rows, two columns instead of
// eight. Same predicates, same run, same session as the full read above it.
func BenchmarkGenScan1000_Contact(b *testing.B) {
	ex := pgxdrv.Pool{P: pool}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	q := genuser.New().Where(genuser.Status.Eq("active"))
	var sl runtime.Slab
	buf := make([]genuser.ContactRow, 0, 1000)
	var err error
	for b.Loop() {
		if buf, err = q.AllContactInto(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
		if len(buf) != 1000 {
			b.Fatal(len(buf))
		}
	}
}

// BenchmarkGenGetOne is the single-row read: the worst case for any per-query
// fixed cost, since nothing amortises it over rows. It is the measurement the
// wire-format guard (docs/PRODUCTION-READINESS.md P0.1) had to clear.
func BenchmarkGenGetOne(b *testing.B) {
	ctx, ex := context.Background(), genExec()
	q := genuser.New().StatusEq("active").Limit(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, ok, err := q.One(ctx, ex)
		if err != nil {
			b.Fatal(err)
		}
		if !ok {
			b.Fatal("no row")
		}
	}
}
