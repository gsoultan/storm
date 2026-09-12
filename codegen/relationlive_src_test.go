package codegen_test

// relationsLiveSrc runs inside the generated CONTEXT package: two tables, a
// foreign key and a named plan, loaded against a real MySQL-family server.
//
// The single-table end-to-end proves CRUD. It cannot prove the batch loader,
// which is the construct M9's exit gate names and the one that is genuinely
// different here — PostgreSQL unnests an array, MySQL has to reach the same
// answer through JSON_TABLE and MariaDB through a window. Those forms PREPARE
// in the shell gate; nothing had ever checked the ROWS they return.
const relationsLiveSrc = `package PKG_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"

	ctxpkg "IMPORTPATH"
	"IMPORTPATH/mdauthor"
	"IMPORTPATH/mdpost"
)

type mdAuthor struct {
	storm.Model
	Name  string
	Posts []mdPost
}

func (a *mdAuthor) Schema(t *storm.Table) { t.Col(&a.Name).Size(80) }
func (a *mdAuthor) Plans(p *storm.Plans)  { p.Named("Feed").With(&a.Posts) }

type mdPost struct {
	storm.Model
	Title  string
	Views  int64
	Author mdAuthor
}

func (p *mdPost) Schema(t *storm.Table) { t.Col(&p.Title).Size(120) }

var ex *mydrv.Pool

func TestMain(m *testing.M) {
	addr := os.Getenv("ADDRVAR")
	if addr == "" {
		os.Stderr.WriteString("ADDRVAR did not reach the generated package\n")
		os.Exit(1)
	}
	ctx := context.Background()
	p, err := mydrv.NewPool(ctx, mydrv.Config{
		Addr: addr, User: "root", Password: "storm", Database: "storm",
		AllowCleartextPasswordOverPlaintext: true,
	})
	must(err)
	defer p.Close()
	ex = p

	s, err := storm.Build(&mdAuthor{}, &mdPost{})
	must(err)
	ddl, err := myddl.CreateFor(s, myddl.TARGET)
	must(err)
	// Children first: the foreign key points the other way.
	for _, t := range []string{"md_posts", "md_authors"} {
		_, _ = p.Exec(ctx, "DROP TABLE IF EXISTS " + "` + "`" + `" + t + "` + "`" + `", nil)
	}
	for _, stmt := range splitDDL(ddl) {
		if _, err := p.Exec(ctx, stmt, nil); err != nil {
			panic(stmt + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func splitDDL(ddl string) []string {
	var out []string
	last := 0
	for i := 0; i < len(ddl); i++ {
		if ddl[i] == ';' {
			if t := trim(ddl[last:i]); t != "" {
				out = append(out, t)
			}
			last = i + 1
		}
	}
	if t := trim(ddl[last:]); t != "" {
		out = append(out, t)
	}
	return out
}

func trim(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\n' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

func id(a, b byte) [16]byte {
	var v [16]byte
	v[0], v[15] = a, b
	return v
}

// The batch loader, end to end: three parents with different child counts, one
// round trip for the parents and one for every child. A loader that returned
// the right SQL and the wrong ROWS passes every test before this one.
func TestPlanLoadsEveryChildInTwoRoundTrips(t *testing.T) {
	ctx := context.Background()
	counted := &countingExecutor{Executor: ex}

	want := map[byte]int{1: 3, 2: 1, 3: 0}
	for a, n := range want {
		author := &mdauthor.Row{ID: id(a, a), Name: "author" + string(rune('A'+a-1))}
		if err := mdauthor.Insert(ctx, ex, author); err != nil {
			t.Fatalf("insert author: %v", err)
		}
		for i := 0; i < n; i++ {
			post := &mdpost.Row{
				ID: id(a, byte(0x40+i)), Title: "post", Views: int64(10 * (i + 1)),
				AuthorID: author.ID,
			}
			if err := mdpost.Insert(ctx, ex, post); err != nil {
				t.Fatalf("insert post: %v", err)
			}
		}
	}

	rows, err := ctxpkg.MdAuthorFeed().Order(mdauthor.ID.Asc()).All(ctx, counted)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d parents, want 3", len(rows))
	}
	for _, r := range rows {
		if got, w := len(r.Posts), want[r.ID[0]]; got != w {
			t.Errorf("author %d loaded %d posts, want %d", r.ID[0], got, w)
		}
		for _, p := range r.Posts {
			// The child must belong to ITS parent. A loader that partitioned
			// wrongly still returns the right TOTAL.
			if p.AuthorID != r.ID {
				t.Errorf("author %d got a post belonging to %v", r.ID[0], p.AuthorID)
			}
		}
	}
	// The whole point of a plan: constant round trips, not one per parent.
	if counted.n != 2 {
		t.Errorf("the plan cost %d round trips, want 2", counted.n)
	}
}

// Greatest-n-per-group, which is the construct M9's exit gate names and the one
// the plain plan above does NOT reach: it uses an In predicate, while ChildTop
// goes through the batch loader — JSON_TABLE on MySQL, a window on MariaDB.
//
// The parent key is a uuid, which is BINARY(16), which cannot travel inside a
// JSON document as itself. That is the default key of every storm model, so
// this is the ordinary case rather than an exotic one.
func TestChildTopUsesTheBatchLoader(t *testing.T) {
	ctx := context.Background()
	counted := &countingExecutor{Executor: ex}
	// The per-relation plan, not the named one: ChildTop belongs to the single
	// relation it limits, and a named plan spanning several has no one child
	// to apply it to.
	rows, err := ctxpkg.MdAuthorWithPosts().
		Order(mdauthor.ID.Asc()).
		ChildOrder(mdpost.ID.Asc()).
		ChildTop(2).
		All(ctx, counted)
	if err != nil {
		t.Fatalf("child-top plan: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d parents, want 3", len(rows))
	}
	// Author 1 has three posts and must come back with two; the limit is
	// expressed in SQL, so slicing afterwards would not be this test passing.
	want := map[byte]int{1: 2, 2: 1, 3: 0}
	for _, r := range rows {
		if got := len(r.Posts); got != want[r.ID[0]] {
			t.Errorf("author %d kept %d posts, want %d", r.ID[0], got, want[r.ID[0]])
		}
		for _, p := range r.Posts {
			if p.AuthorID != r.ID {
				t.Errorf("author %d got a post belonging to %v", r.ID[0], p.AuthorID)
			}
		}
	}
	if counted.n != 2 {
		t.Errorf("the child-top plan cost %d round trips, want 2", counted.n)
	}
}

// A read across the foreign key, which is the other thing two tables buy.
func TestChildQueryFiltersByItsParent(t *testing.T) {
	ctx := context.Background()
	got, err := mdpost.New().Where(mdpost.AuthorID.Eq(id(1, 1))).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("read %d posts for author 1, want 3", len(got))
	}
	none, err := mdpost.New().Where(mdpost.AuthorID.Eq(id(3, 3))).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("read %d posts for a parent with none", len(none))
	}
}

// Keyset pagination over the plan's parents: the cursor is a row the caller was
// given, and the second page must not repeat the first.
func TestKeysetPagingOverTheParents(t *testing.T) {
	ctx := context.Background()
	p1, err := ctxpkg.MdAuthorFeed().Order(mdauthor.ID.Asc()).Limit(2).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 2 {
		t.Fatalf("page one has %d rows, want 2", len(p1))
	}
	p2, err := ctxpkg.MdAuthorFeed().Order(mdauthor.ID.Asc()).
		After(p1[len(p1)-1]).Limit(2).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2) != 1 {
		t.Fatalf("page two has %d rows, want 1", len(p2))
	}
	if p2[0].ID == p1[0].ID || p2[0].ID == p1[1].ID {
		t.Error("the second page repeats a row from the first")
	}
}

// countingExecutor counts round trips, which is the claim a plan makes.
type countingExecutor struct {
	runtime.Executor
	n int
}

func (c *countingExecutor) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c.n++
	return c.Executor.Query(ctx, sql, args)
}

// A declared JOIN, run rather than spelled. Its SQL has been asserted as text
// since compile/mysql landed; nothing had checked which rows come back or
// whether the ON clause correlates the right columns.
func TestDeclaredJoinReturnsBothSidesRows(t *testing.T) {
	ctx := context.Background()
	got, err := ctxpkg.MdPostWithAuthor().All(ctx, ex)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	// Four posts across three authors; the author with none contributes no row
	// to an inner join, which is the join actually being a join.
	if len(got) != 4 {
		t.Fatalf("the join read %d rows, want 4", len(got))
	}
	for _, r := range got {
		// The joined row embeds BOTH sides, so the far side's columns are
		// there under its own row type rather than flattened into names.
		if r.Author.Name == "" {
			t.Errorf("a joined row has no author name: %+v", r)
		}
		if r.Title == "" {
			t.Errorf("a joined row has no title: %+v", r)
		}
	}
}

// A declared AGGREGATE, likewise: GROUP BY, an aggregate function and a HAVING,
// against real rows.
func TestDeclaredAggregateGroupsAndFilters(t *testing.T) {
	ctx := context.Background()
	got, err := mdpost.New().AllByAuthor(ctx, ex)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	// Two authors have posts; the third is absent because a group with no rows
	// is no group, and HAVING count > 0 cannot resurrect it.
	if len(got) != 2 {
		t.Fatalf("the aggregate read %d groups, want 2", len(got))
	}
	byCount := map[int64]string{}
	top := map[int64]int64{}
	for _, g := range got {
		// SUM over an integer column comes back as a DECIMAL on this engine,
		// which is why the generated field is a Decimal and not an int64 — a
		// sum can exceed the summed type.
		byCount[g.Posts] = g.Views.V.String()
		top[g.Posts] = g.TopViews.V
	}
	// Author 1 has three posts with 10, 20 and 30 views; author 2 has one with 10.
	if v := byCount[3]; v != "60" {
		t.Errorf("the three-post group sums to %s, want 60 (groups: %+v)", v, got)
	}
	if v := byCount[1]; v != "10" {
		t.Errorf("the one-post group sums to %s, want 10 (groups: %+v)", v, got)
	}
	if top[3] != 30 {
		t.Errorf("the three-post group's max is %d, want 30", top[3])
	}
}

// A SEMI-JOIN: "authors who have a post", which must not multiply the parent by
// its children the way a join would.
func TestSemiJoinDoesNotMultiplyTheParent(t *testing.T) {
	ctx := context.Background()
	got, err := ctxpkg.MdAuthorHavingPosts(mdauthor.New()).All(ctx, ex)
	if err != nil {
		t.Fatalf("semi-join: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("read %d authors with posts, want 2 — a join would have said 4", len(got))
	}
	none, err := ctxpkg.MdAuthorNotHavingPosts(mdauthor.New()).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 1 {
		t.Errorf("read %d authors without posts, want 1", len(none))
	}
}

// A UNION, which on this back end has bare placeholders and a per-branch
// soft-delete predicate. Ordering applies to the MERGE, which is the one thing
// a per-table query cannot give.
func TestUnionMergesBothTablesInOneOrder(t *testing.T) {
	ctx := context.Background()
	got, err := ctxpkg.Names(ctx, ex, 100)
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	// Three authors and four posts.
	if len(got) != 7 {
		t.Fatalf("the union read %d rows, want 7", len(got))
	}
	kinds := map[string]int{}
	for _, r := range got {
		kinds[r.Kind]++
	}
	if kinds["author"] != 3 || kinds["post"] != 4 {
		t.Errorf("the union produced %v, want 3 authors and 4 posts", kinds)
	}
	// The ordering applies across both branches, not within each.
	for i := 1; i < len(got); i++ {
		if got[i-1].Text > got[i].Text {
			t.Errorf("the merge is not ordered: %q then %q", got[i-1].Text, got[i].Text)
			break
		}
	}
}

// A row lock inside a transaction, through the generated API.
//
// MySQL spells FOR SHARE and MariaDB spells LOCK IN SHARE MODE, and the clause
// goes at the very end after LIMIT. Nothing had run one: the compile tests
// assert the text and the shell gate PREPAREs it, but a lock that is not taken
// inside a transaction is a no-op, and only a transaction can show it works.
func TestRowLockingInsideATransaction(t *testing.T) {
	ctx := context.Background()
	tx, err := ex.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	got, err := mdauthor.New().Where(mdauthor.ID.Eq(id(1, 1))).ForUpdate().All(ctx, tx, nil)
	if err != nil {
		t.Fatalf("FOR UPDATE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the locked read returned %d rows, want 1", len(got))
	}
	// SKIP LOCKED is the one a queue worker needs, and the one whose absence
	// turns a claim into a deadlock.
	skipped, err := mdauthor.New().Where(mdauthor.ID.Eq(id(1, 1))).
		ForUpdateSkipLocked().All(ctx, tx, nil)
	if err != nil {
		t.Fatalf("FOR UPDATE SKIP LOCKED: %v", err)
	}
	if len(skipped) != 1 {
		t.Errorf("the skip-locked read returned %d rows in its own transaction, want 1",
			len(skipped))
	}
	// The shared form, which is where the two engines spell it differently.
	shared, err := mdauthor.New().Where(mdauthor.ID.Eq(id(1, 1))).ForShare().All(ctx, tx, nil)
	if err != nil {
		t.Fatalf("FOR SHARE: %v", err)
	}
	if len(shared) != 1 {
		t.Errorf("the shared read returned %d rows, want 1", len(shared))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
`
