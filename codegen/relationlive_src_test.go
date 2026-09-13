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
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"

	ctxpkg "IMPORTPATH"
	"IMPORTPATH/mdattachment"
	"IMPORTPATH/mdauthor"
	"IMPORTPATH/mdnode"
	"IMPORTPATH/mdpost"
	"IMPORTPATH/mdtag"
	"IMPORTPATH/mdwide"
)

type mdAuthor struct {
	storm.Model
	Name  string
	Posts []mdPost
}

func (a *mdAuthor) Schema(t *storm.Table) { t.Col(&a.Name).Size(80) }

// A declared column subset, which has its own scan path and its own statement.
func (a *mdAuthor) Projections(p *storm.Projections) { p.Named("Card", &a.Name) }
func (a *mdAuthor) Plans(p *storm.Plans)  { p.Named("Feed").With(&a.Posts) }

type mdPost struct {
	storm.Model
	Title     string
	Views     int64
	DeletedAt *time.Time
	Author    mdAuthor
	Tags      []mdTag
}

func (p *mdPost) Schema(t *storm.Table) {
	// Soft delete on the CHILD, so the cross-table reads have something to
	// exclude: a plan, a join, an aggregate and a union each have to carry the
	// predicate to the right alias, which is a different problem from carrying
	// it on a single-table read.
	t.SoftDelete(&p.DeletedAt)
	t.Col(&p.Title).Size(120)
}

// The implicit MANY-TO-MANY: a slice on both sides and storm generates the join
// table nobody declared. Its loader is a two-hop read no other test reaches.
type mdTag struct {
	storm.Model
	Label string
	Posts []mdPost
}

func (g *mdTag) Schema(t *storm.Table) { t.Col(&g.Label).Size(40) }

// The polymorphic ARC: exactly one of the variants is set, enforced by the
// database rather than by the caller. Its loader batches per variant, and the
// CHECK that enforces exactly-one is a construct MySQL only gained in 8.0.16.
type mdAttachment struct {
	storm.Model
	Filename string
	Subject  storm.OneOf2[mdAuthor, mdPost]
}

func (a *mdAttachment) Schema(t *storm.Table) { t.Col(&a.Filename).Size(120) }

// Every scalar type that ports, so the GENERATED SCANNER for each one is
// exercised against real server bytes. The driver's own round trip covers the
// decoders; this covers the code that calls them, which is a different path and
// the one every read goes through.
type mdWide struct {
	storm.Model
	Flag    bool
	Small   int16
	Medium  int32
	Big     int64
	Single  float32
	Double  float64
	Text    string
	Blob    []byte
	Stamp   time.Time
	Day     time.Time
	Clock   storm.TimeOfDay
	Money   storm.Decimal
	Doc     storm.JSON
	OptText *string
	OptBig  *int64
	OptDay  *time.Time
}

func (w *mdWide) Schema(t *storm.Table) {
	t.Col(&w.Text).Size(80)
	t.Col(&w.Day).Date()
	t.Col(&w.Money).Numeric(18, 6)
}

type mdNode struct {
	storm.Model
	Name     string
	Parent   *mdNode
	Children []mdNode
}

func (n *mdNode) Schema(t *storm.Table) {
	t.Col(&n.Name).Size(60)
	t.Col(&n.Parent).OnDelete(storm.Cascade)
}

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

	s, err := storm.Build(&mdAuthor{}, &mdPost{}, &mdNode{}, &mdTag{}, &mdAttachment{}, &mdWide{})
	must(err)
	ddl, err := myddl.CreateFor(s, myddl.TARGET)
	must(err)
	// Foreign-key checks off for the drops. Dropping in dependency order works
	// until a table is added and the order is not updated with it — and the
	// symptom is "table already exists" on the CREATE, which names the wrong
	// table. Off, drop everything, on.
	_, _ = p.Exec(ctx, "SET FOREIGN_KEY_CHECKS = 0", nil)
	for _, t := range []string{
		"md_attachments", "md_post_md_tags", "md_tags", "md_posts", "md_authors", "md_nodes",
		"md_wides",
	} {
		if _, err := p.Exec(ctx, "DROP TABLE IF EXISTS "+t, nil); err != nil {
			panic("drop " + t + ": " + err.Error())
		}
	}
	_, _ = p.Exec(ctx, "SET FOREIGN_KEY_CHECKS = 1", nil)
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

// WITH RECURSIVE, run rather than spelled.
//
// This is the construct with the LEAST coverage of anything storm generates for
// this target: the shell gates never PREPAREd it, so before this it had none of
// any kind. It also takes its roots as a bound key list, which is the exact
// path that broke every fetch plan, and its cycle guard is where the two
// engines part company hardest — PostgreSQL accumulates visited keys in an
// ARRAY, MySQL has no array type and joins HEX strings with FIND_IN_SET.
func TestRecursiveDescendsAndAscends(t *testing.T) {
	ctx := context.Background()
	// A 1 -> 2 -> 4 chain with a sibling 3 under 1, so depth and breadth are
	// distinguishable and a bound that counted wrongly shows up as a count.
	//
	//   1
	//   |- 2 - 4
	//   \- 3
	tree := []struct {
		id     byte
		name   string
		parent byte // 0 = root
	}{{1, "root", 0}, {2, "child", 1}, {3, "sibling", 1}, {4, "grandchild", 2}}
	for _, n := range tree {
		row := &mdnode.Row{ID: id(0x70, n.id), Name: n.name}
		if n.parent != 0 {
			p := id(0x70, n.parent)
			row.ParentID = runtime.Null[[16]byte]{V: p, Valid: true}
		}
		if err := mdnode.Insert(ctx, ex, row); err != nil {
			t.Fatalf("insert %s: %v", n.name, err)
		}
	}

	// The roots are included AT DEPTH 1, so maxDepth 1 returns exactly them.
	only, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 1)
	if err != nil {
		t.Fatalf("descend depth 1: %v", err)
	}
	if len(only) != 1 || only[0].Name != "root" {
		t.Fatalf("depth 1 returned %d rows (%+v), want just the root", len(only), only)
	}

	// Depth 2 adds one level: the two children, not the grandchild.
	two, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 2)
	if err != nil {
		t.Fatalf("descend depth 2: %v", err)
	}
	if len(two) != 3 {
		t.Fatalf("depth 2 returned %d rows, want 3 (root + two children): %+v", len(two), two)
	}
	if namesOf(two)["grandchild"] {
		t.Error("the depth bound did not hold: a grandchild came back at depth 2")
	}

	// The whole subtree.
	all, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 10)
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("the subtree has %d rows, want 4: %+v", len(all), all)
	}

	// Upward, from the deepest row: itself, its parent, and the root.
	up, err := mdnode.Ascend(ctx, ex, [][16]byte{id(0x70, 4)}, 10)
	if err != nil {
		t.Fatalf("ascend: %v", err)
	}
	got := namesOf(up)
	if len(up) != 3 || !got["grandchild"] || !got["child"] || !got["root"] {
		t.Errorf("the ancestor chain is %+v, want grandchild, child and root", up)
	}
	if got["sibling"] {
		t.Error("ascending reached a sibling, which is not an ancestor")
	}

	// A traversal with no positive bound is refused rather than run: unbounded
	// recursion over a cycle does not return.
	if _, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 0); err == nil {
		t.Error("an unbounded traversal was accepted")
	}
	// ...and so is one deeper than the cycle guard can hold. This back end
	// accumulates visited keys in a fixed-width column, and past its limit the
	// path overflows — an error in strict mode and a SILENT TRUNCATION without
	// it, which is a guard that stops guarding. Refused here rather than
	// trusted to the server's sql_mode.
	if _, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 100000); err == nil {
		t.Error("a traversal deeper than the cycle guard was accepted")
	} else if !errors.Is(err, mdnode.ErrDepthTooDeep) {
		t.Errorf("err = %v, want ErrDepthTooDeep", err)
	}
}

// The cycle guard, which is the half that differs most between the engines.
//
// A foreign key does not stop A pointing at B pointing at A. Without a guard
// the query runs to the server's recursion limit and the connection hangs; the
// guard has to stop it at the point it revisits a key.
func TestRecursiveTerminatesOnACycle(t *testing.T) {
	ctx := context.Background()
	// Two rows pointing at each other. Inserted with NULL parents first,
	// because each references the other and neither can be second.
	a, b := id(0x71, 1), id(0x71, 2)
	for _, n := range []struct {
		id   [16]byte
		name string
	}{{a, "cycle-a"}, {b, "cycle-b"}} {
		if err := mdnode.Insert(ctx, ex, &mdnode.Row{ID: n.id, Name: n.name}); err != nil {
			t.Fatalf("insert %s: %v", n.name, err)
		}
	}
	for _, l := range []struct{ from, to [16]byte }{{a, b}, {b, a}} {
		// Unquoted identifiers: neither is reserved, and a backtick would end
		// the template this source lives in.
		if _, err := ex.Exec(ctx,
			"UPDATE md_nodes SET parent_id = ? WHERE id = ?", []any{l.to, l.from}); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	done := make(chan struct{})
	var rows []mdnode.Row
	var err error
	go func() {
		defer close(done)
		rows, err = mdnode.Descend(ctx, ex, [][16]byte{a}, 50)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the traversal did not terminate on a cycle")
	}
	if err != nil {
		t.Fatalf("descend over a cycle: %v", err)
	}
	// Each row once: the guard stops at the revisit rather than at the depth
	// bound, so 50 levels of a two-node cycle is two rows, not fifty.
	if len(rows) != 2 {
		t.Errorf("the cycle produced %d rows, want 2 — the guard stopped at the depth "+
			"bound rather than at the revisited key: %+v", len(rows), rows)
	}
}

func namesOf(rows []mdnode.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Name] = true
	}
	return out
}

// A declared PROJECTION: a column subset with its own statement and its own
// scan path, neither of which any other test reaches.
func TestProjectionReadsItsSubset(t *testing.T) {
	ctx := context.Background()
	got, err := mdauthor.New().Order(mdauthor.ID.Asc()).AllCard(ctx, ex)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the projection read %d rows, want 3", len(got))
	}
	for _, r := range got {
		if r.Name == "" {
			t.Errorf("a projected row has no name: %+v", r)
		}
	}
}

// The UNIT OF WORK, whose whole claim is that it orders a flush by foreign key
// so a child written before its parent still lands. Declaration order here is
// deliberately wrong.
func TestUnitFlushesInForeignKeyOrder(t *testing.T) {
	ctx := context.Background()
	authorID, postID := id(0x80, 1), id(0x80, 2)
	u := ctxpkg.NewUnit()
	// Child first. A flush in declaration order violates md_posts.author_id.
	u.Add(mdpost.Table, mdpost.InsertOp(mdpost.Row{
		ID: postID, Title: "unit", Views: 5, AuthorID: authorID,
	}))
	u.Add(mdauthor.Table, mdauthor.InsertOp(mdauthor.Row{ID: authorID, Name: "unit-author"}))
	t.Cleanup(func() {
		_ = mdpost.Delete(ctx, ex, postID)
		_ = mdauthor.Delete(ctx, ex, authorID)
	})

	affected, err := u.Flush(ctx, ex)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(affected) != 2 {
		t.Fatalf("the flush reported %d results, want 2", len(affected))
	}
	for i, n := range affected {
		if n != 1 {
			t.Errorf("statement %d affected %d rows, want 1", i, n)
		}
	}
	if got := countPosts(t, mdpost.New().Where(mdpost.ID.Eq(postID))); got != 1 {
		t.Errorf("the child did not land: %d rows", got)
	}
}

// SOFT DELETE across the cross-table reads, which is a different problem from
// carrying the predicate on a single-table read: each of these has to attach it
// to the right ALIAS, and a union has to attach it per BRANCH.
func TestSoftDeleteReachesEveryCrossTableRead(t *testing.T) {
	ctx := context.Background()
	// Counted BEFORE and after, not against absolute numbers: these tests share
	// a database and an earlier one's rows are not this one's business. A
	// delta of exactly one is the claim either way.
	before := crossTableCounts(t)

	// Author 2's only post, so its disappearance is unambiguous.
	victim := id(2, 0x40)
	if err := mdpost.Delete(ctx, ex, victim); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// The row survives — that is what makes it a soft delete — but no read
	// returns it.
	if got := countPosts(t, mdpost.New().Where(mdpost.ID.Eq(victim))); got != 0 {
		t.Errorf("a soft-deleted row still reads back from its own table")
	}

	after := crossTableCounts(t)
	for _, k := range []string{"plan", "batchLoader", "join", "union"} {
		if got := before[k] - after[k]; got != 1 {
			t.Errorf("%s: %d fewer rows after one soft delete, want 1 — "+
				"the predicate did not reach it (%d then %d)",
				k, got, before[k], after[k])
		}
	}
	// Author 2's only post is gone, so its GROUP disappears rather than
	// counting zero, and the semi-join stops matching it.
	for _, k := range []string{"aggregate", "semiJoin"} {
		if got := before[k] - after[k]; got != 1 {
			t.Errorf("%s: %d fewer after one soft delete, want 1 (%d then %d)",
				k, got, before[k], after[k])
		}
	}
}

// crossTableCounts is every read that spans tables, counted once.
func crossTableCounts(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	out := map[string]int{}

	rows, err := ctxpkg.MdAuthorFeed().Order(mdauthor.ID.Asc()).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		out["plan"] += len(r.Posts)
	}

	// A different statement from the plan's In: this one is the batch loader.
	top, err := ctxpkg.MdAuthorWithPosts().Order(mdauthor.ID.Asc()).
		ChildOrder(mdpost.ID.Asc()).ChildTop(50).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range top {
		out["batchLoader"] += len(r.Posts)
	}

	joined, err := ctxpkg.MdPostWithAuthor().All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	out["join"] = len(joined)

	groups, err := mdpost.New().AllByAuthor(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	out["aggregate"] = len(groups)

	names, err := ctxpkg.Names(ctx, ex, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out["union"] = len(names)

	having, err := ctxpkg.MdAuthorHavingPosts(mdauthor.New()).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	out["semiJoin"] = len(having)
	return out
}

func countPosts(t *testing.T, q mdpost.Query) int64 {
	t.Helper()
	n, err := q.Count(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The implicit MANY-TO-MANY, whose loader is a two-hop read: parents, then the
// join table joined to the far side. Nothing else generates that shape.
func TestManyToManyLoadsBothDirections(t *testing.T) {
	ctx := context.Background()
	tags := []struct {
		id    byte
		label string
	}{{1, "go"}, {2, "sql"}}
	for _, g := range tags {
		if err := mdtag.Insert(ctx, ex, &mdtag.Row{ID: id(0x90, g.id), Label: g.label}); err != nil {
			t.Fatalf("insert tag: %v", err)
		}
	}
	// Post 1 carries both tags; post 2 carries one. The join table has no model,
	// so it is written through the generated link helpers.
	post1, post2 := id(1, 0x40), id(1, 0x41)
	for _, l := range []struct{ post, tag [16]byte }{
		{post1, id(0x90, 1)}, {post1, id(0x90, 2)}, {post2, id(0x90, 1)},
	} {
		if _, err := ex.Exec(ctx,
			"INSERT INTO md_post_md_tags (md_post_id, md_tag_id) VALUES (?, ?)",
			[]any{l.post, l.tag}); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_post_md_tags", nil) })

	posts, err := ctxpkg.MdPostWithTags().Order(mdpost.ID.Asc()).All(ctx, ex)
	if err != nil {
		t.Fatalf("post -> tags: %v", err)
	}
	byPost := map[[16]byte]int{}
	for _, p := range posts {
		byPost[p.ID] = len(p.Tags)
	}
	if byPost[post1] != 2 {
		t.Errorf("post 1 has %d tags, want 2", byPost[post1])
	}
	if byPost[post2] != 1 {
		t.Errorf("post 2 has %d tags, want 1", byPost[post2])
	}

	// And the other way, which is a different generated loader rather than the
	// same one read backwards.
	tagRows, err := ctxpkg.MdTagWithPosts().Order(mdtag.ID.Asc()).All(ctx, ex)
	if err != nil {
		t.Fatalf("tag -> posts: %v", err)
	}
	if len(tagRows) != 2 {
		t.Fatalf("read %d tags, want 2", len(tagRows))
	}
	if len(tagRows[0].Posts) != 2 {
		t.Errorf("tag 'go' has %d posts, want 2", len(tagRows[0].Posts))
	}
	if len(tagRows[1].Posts) != 1 {
		t.Errorf("tag 'sql' has %d posts, want 1", len(tagRows[1].Posts))
	}
}

// The polymorphic ARC: exactly one variant, enforced by the DATABASE, and a
// loader that batches per variant.
func TestArcLoadsEveryVariantAndEnforcesExactlyOne(t *testing.T) {
	ctx := context.Background()
	// One attachment per variant.
	toAuthor := mdattachment.Create()
	toAuthor.SetFilename("author.txt")
	toAuthor.SetMdAuthorID(id(1, 1))
	if _, err := toAuthor.Insert(ctx, ex); err != nil {
		t.Fatalf("attach to author: %v", err)
	}
	toPost := mdattachment.Create()
	toPost.SetFilename("post.txt")
	toPost.SetMdPostID(id(1, 0x40))
	if _, err := toPost.Insert(ctx, ex); err != nil {
		t.Fatalf("attach to post: %v", err)
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_attachments", nil) })

	// BOTH variants set, and neither: the CHECK is the database's, so a caller
	// cannot write a row that means two things or nothing.
	both := mdattachment.Create()
	both.SetFilename("both.txt")
	both.SetMdAuthorID(id(1, 1))
	both.SetMdPostID(id(1, 0x40))
	if _, err := both.Insert(ctx, ex); err == nil {
		t.Error("two variants at once passed the exactly-one CHECK")
	}
	none := mdattachment.Create()
	none.SetFilename("none.txt")
	if _, err := none.Insert(ctx, ex); err == nil {
		t.Error("no variant at all passed the exactly-one CHECK")
	}

	// The loader resolves each row to its own variant, in one round trip per
	// variant rather than one per row.
	counted := &countingExecutor{Executor: ex}
	rows, err := ctxpkg.MdAttachmentWithSubject().Order(mdattachment.ID.Asc()).All(ctx, counted)
	if err != nil {
		t.Fatalf("arc loader: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d attachments, want 2", len(rows))
	}
	var sawAuthor, sawPost bool
	for _, r := range rows {
		switch {
		case r.MdAuthorID.Valid:
			sawAuthor = true
		case r.MdPostID.Valid:
			sawPost = true
		default:
			t.Errorf("an attachment resolved to no variant: %+v", r)
		}
	}
	if !sawAuthor || !sawPost {
		t.Error("the loader did not resolve both variants")
	}
	// One query for the parents and one per variant, not one per row.
	if counted.n > 3 {
		t.Errorf("the arc loader cost %d round trips, want at most 3", counted.n)
	}
}

// The key storm has to supply itself on this target.
//
// PostgreSQL defaults it with gen_random_uuid(); MySQL and MariaDB have no
// expression worth taking — UUID() is version 1 and embeds the server's MAC
// address in a value that ends up in URLs — so the generated write path fills
// it. Without that a caller who relied on the default got sixteen zero bytes,
// which makes the SECOND insert a duplicate and the first row unfindable.
func TestAKeyIsGeneratedWhenTheCallerDoesNotSetOne(t *testing.T) {
	ctx := context.Background()
	n := mdauthor.Create()
	n.SetName("no-id-given")
	row, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatalf("insert with no id: %v", err)
	}
	if row.ID == ([16]byte{}) {
		t.Fatal("the row came back with sixteen zero bytes for a primary key")
	}
	t.Cleanup(func() { _ = mdauthor.Delete(ctx, ex, row.ID) })

	// Version 7, so the key stays time-ordered: on this engine the primary key
	// IS the clustered index.
	if v := row.ID[6] >> 4; v != 7 {
		t.Errorf("uuid version = %d, want 7", v)
	}

	// It is the key that actually landed, not one invented for the return.
	got, ok, err := mdauthor.New().IDEq(row.ID).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Name != "no-id-given" {
		t.Errorf("the row is not findable by the key that was returned: %+v", got)
	}

	// A second insert must not collide, which sixteen zero bytes would.
	n2 := mdauthor.Create()
	n2.SetName("also-no-id")
	row2, err := n2.Insert(ctx, ex)
	if err != nil {
		t.Fatalf("second insert with no id: %v", err)
	}
	t.Cleanup(func() { _ = mdauthor.Delete(ctx, ex, row2.ID) })
	if row2.ID == row.ID {
		t.Error("two inserts produced the same key")
	}

	// An id the caller DID set still wins: this supplies a missing key, it does
	// not overrule a given one.
	mine := id(0xa0, 1)
	n3 := mdauthor.Create()
	n3.SetID(mine)
	n3.SetName("mine")
	row3, err := n3.Insert(ctx, ex)
	if err != nil {
		t.Fatalf("insert with an id: %v", err)
	}
	t.Cleanup(func() { _ = mdauthor.Delete(ctx, ex, mine) })
	if row3.ID != mine {
		t.Errorf("the caller's id was replaced: %x", row3.ID)
	}
}

// The UPDATE path, which the single-table end-to-end's name claims and does
// not do: it is called TestInsertSelectUpdateDelete and never updates.
func TestUpdateWritesOnlyWhatWasSet(t *testing.T) {
	ctx := context.Background()
	r := &mdpost.Row{ID: id(0xb0, 1), Title: "before", Views: 1, AuthorID: id(1, 1)}
	if err := mdpost.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_posts WHERE id = ?", []any{r.ID}) })

	m := mdpost.Mutate(*r)
	m.SetTitle("after")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, ok, err := mdpost.New().Where(mdpost.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("read back: %v ok=%v", err, ok)
	}
	if got.Title != "after" {
		t.Errorf("title = %q, want after", got.Title)
	}
	// A masked update writes the columns that were SET and no others: an
	// update that rewrote every column would clobber a concurrent writer's
	// change to a field this caller never touched.
	if got.Views != 1 {
		t.Errorf("views = %d, want 1 — an unset column was rewritten", got.Views)
	}

	// A row that is not there affects nothing, and says so rather than
	// reporting a silent success.
	gone := mdpost.Mutate(mdpost.Row{ID: id(0xb0, 9), AuthorID: id(1, 1)})
	gone.SetTitle("nobody")
	if err := gone.Update(ctx, ex); err == nil {
		t.Error("updating a row that does not exist reported success")
	}
}

// A soft-deleted row must not be updatable: it is gone as far as every read is
// concerned, and an update that still reached it would resurrect a value
// nobody can see.
func TestUpdateSkipsASoftDeletedRow(t *testing.T) {
	ctx := context.Background()
	r := &mdpost.Row{ID: id(0xb1, 1), Title: "doomed", AuthorID: id(1, 1)}
	if err := mdpost.Insert(ctx, ex, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_posts WHERE id = ?", []any{r.ID}) })
	if err := mdpost.Delete(ctx, ex, r.ID); err != nil {
		t.Fatal(err)
	}
	m := mdpost.Mutate(*r)
	m.SetTitle("resurrected")
	if err := m.Update(ctx, ex); err == nil {
		t.Error("a soft-deleted row was updated")
	}
}

// The bulk path. On this target CopyFrom is emulated with a multi-row INSERT —
// one round trip, but a different statement from the per-row form.
func TestInsertAllLoadsEveryRow(t *testing.T) {
	ctx := context.Background()
	rows := make([]mdpost.Row, 50)
	for i := range rows {
		rows[i] = mdpost.Row{
			ID: id(0xc0, byte(i)), Title: "bulk", Views: int64(i), AuthorID: id(1, 1),
		}
	}
	t.Cleanup(func() {
		_, _ = ex.Exec(ctx, "DELETE FROM md_posts WHERE title = ?", []any{"bulk"})
	})
	n, err := mdpost.InsertAll(ctx, ex, rows)
	if err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if n != 50 {
		t.Errorf("InsertAll reported %d rows, want 50", n)
	}
	got, err := mdpost.New().Where(mdpost.Title.Eq("bulk")).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Errorf("%d rows landed, want 50", got)
	}
}

// AnyOf ORs whole conjunctions. Its parenthesisation is the thing to get wrong:
// "a AND b OR c AND d" without brackets binds differently and returns rows
// nobody asked for, with no error to notice.
func TestAnyOfBracketsItsConjunctions(t *testing.T) {
	ctx := context.Background()
	// (title = 'x' AND views = 1) OR (title = 'y' AND views = 2)
	// Their own author, so the scope is a single equality rather than a range:
	// a uuid column has no ordering predicates, and inventing one would be a
	// comparison of random bytes.
	owner := id(0xd0, 0xff)
	if err := mdauthor.Insert(ctx, ex, &mdauthor.Row{ID: owner, Name: "anyof"}); err != nil {
		t.Fatal(err)
	}
	seed := []struct {
		id    byte
		title string
		views int64
	}{{1, "x", 1}, {2, "y", 2}, {3, "x", 2}, {4, "y", 1}}
	for _, s := range seed {
		if err := mdpost.Insert(ctx, ex, &mdpost.Row{
			ID: id(0xd0, s.id), Title: s.title, Views: s.views, AuthorID: owner,
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = ex.Exec(ctx, "DELETE FROM md_posts WHERE author_id = ?", []any{owner})
		_, _ = ex.Exec(ctx, "DELETE FROM md_authors WHERE id = ?", []any{owner})
	})

	got, err := mdpost.New().
		Where(mdpost.AuthorID.Eq(owner)).
		AnyOf(
			mdpost.And(mdpost.Title.Eq("x"), mdpost.Views.Eq(1)),
			mdpost.And(mdpost.Title.Eq("y"), mdpost.Views.Eq(2)),
		).Count(ctx, ex)
	if err != nil {
		t.Fatalf("AnyOf: %v", err)
	}
	// Exactly the two matching pairs. Mis-bracketed, "x AND 1 OR y AND 2" would
	// still be two here, so the crossed rows are seeded to make it three or
	// four when the brackets are wrong.
	if got != 2 {
		t.Errorf("AnyOf matched %d rows, want 2 — check the bracketing", got)
	}

	// And the negation, which brackets the whole disjunction rather than each
	// branch.
	not, err := mdpost.New().
		Where(mdpost.AuthorID.Eq(owner)).
		NotAnyOf(
			mdpost.And(mdpost.Title.Eq("x"), mdpost.Views.Eq(1)),
			mdpost.And(mdpost.Title.Eq("y"), mdpost.Views.Eq(2)),
		).Count(ctx, ex)
	if err != nil {
		t.Fatalf("NotAnyOf: %v", err)
	}
	if not != 2 {
		t.Errorf("NotAnyOf matched %d rows, want 2", not)
	}
}

// Offset and Unordered: paging past a page, and the read that opts out of the
// default ordering.
func TestOffsetAndUnordered(t *testing.T) {
	ctx := context.Background()
	all, err := mdauthor.New().Order(mdauthor.ID.Asc()).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Skipf("only %d authors", len(all))
	}
	page, err := mdauthor.New().Order(mdauthor.ID.Asc()).Limit(2).Offset(1).All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("offset: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("the offset page has %d rows, want 2", len(page))
	}
	if page[0].ID != all[1].ID {
		t.Errorf("offset 1 started at %x, want %x", page[0].ID, all[1].ID)
	}

	// Unordered drops the ORDER BY rather than replacing it. The rows are
	// whatever the engine returns, so only the COUNT is assertable — which is
	// the honest claim: it must not lose or duplicate rows.
	un, err := mdauthor.New().Unordered().All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("unordered: %v", err)
	}
	if len(un) != len(all) {
		t.Errorf("unordered read %d rows, ordered read %d", len(un), len(all))
	}
}

// Every scalar type that ports, written and read back through the GENERATED
// code. The driver's own round trip covers the decoders; this covers the
// scanner that calls them, which is the path every read takes.
func TestEveryColumnTypeRoundTripsThroughGeneratedCode(t *testing.T) {
	ctx := context.Background()
	stamp := time.Date(2026, 9, 13, 23, 59, 58, 123456000, time.UTC)
	day := time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)
	// MICROSECONDS: runtime.TimeOfDay counts them, not nanoseconds. Negative
	// and over a day, because MySQL TIME is a signed duration and a decoder
	// that models it as a clock reading loses both facts.
	clock := storm.TimeOfDay(-((30*time.Hour + 20*time.Minute + 10*time.Second + 500*time.Millisecond) / time.Microsecond))
	money, err := storm.ParseDecimal("-123456789012.345678")
	if err != nil {
		t.Fatal(err)
	}
	optText := "present"
	optBig := int64(-99)

	w := &mdwide.Row{
		ID: id(0xe0, 1), Flag: true,
		Small: -32768, Medium: 2147483647, Big: -9223372036854775808,
		Single: 0.5, Double: -1.25,
		Text:  "héllo — ünicode",
		Blob:  []byte{0, 1, 2, 0xff},
		Stamp: stamp, Day: day, Clock: clock, Money: money,
		Doc:     storm.JSON("{\"a\": 1, \"b\": [2, 3]}"),
		OptText: runtime.Null[string]{V: optText, Valid: true},
		OptBig:  runtime.Null[int64]{V: optBig, Valid: true},
	}
	if err := mdwide.Insert(ctx, ex, w); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_wides", nil) })

	got, ok, err := mdwide.New().Where(mdwide.ID.Eq(w.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("read back: %v ok=%v", err, ok)
	}
	if got.Flag != true {
		t.Errorf("bool = %v", got.Flag)
	}
	if got.Small != -32768 || got.Medium != 2147483647 || got.Big != -9223372036854775808 {
		t.Errorf("integers = %d %d %d", got.Small, got.Medium, got.Big)
	}
	if got.Single != 0.5 || got.Double != -1.25 {
		t.Errorf("floats = %v %v", got.Single, got.Double)
	}
	if got.Text != "héllo — ünicode" {
		t.Errorf("text = %q", got.Text)
	}
	if string(got.Blob) != string([]byte{0, 1, 2, 0xff}) {
		t.Errorf("blob = %v", got.Blob)
	}
	if !got.Stamp.Equal(stamp) {
		t.Errorf("timestamp = %v, want %v", got.Stamp, stamp)
	}
	if !got.Day.Equal(day) {
		t.Errorf("date = %v, want %v", got.Day, day)
	}
	// A TIME is a signed duration here, not a clock reading: it may exceed a
	// day and it may be negative, and a decoder that models it as a time of day
	// loses both facts.
	if got.Clock != clock {
		t.Errorf("time of day = %v, want %v", time.Duration(got.Clock), time.Duration(clock))
	}
	if got.Money.String() != money.String() {
		t.Errorf("decimal = %s, want %s", got.Money, money)
	}
	if !strings.Contains(string(got.Doc), "\"a\"") {
		t.Errorf("json = %s", got.Doc)
	}
	if !got.OptText.Valid || got.OptText.V != optText {
		t.Errorf("nullable text = %+v", got.OptText)
	}
	if !got.OptBig.Valid || got.OptBig.V != optBig {
		t.Errorf("nullable int = %+v", got.OptBig)
	}
	if got.OptDay.Valid {
		t.Errorf("an unset nullable came back valid: %+v", got.OptDay)
	}

	// A second row with every nullable UNSET, so the NULL path is read through
	// the generated scanner too — and so the bulk insert carries a NULL, which
	// is what used to stop the process.
	empty := mdwide.Row{ID: id(0xe0, 2), Text: "empty", Doc: storm.JSON("{}")}
	if _, err := mdwide.InsertAll(ctx, ex, []mdwide.Row{empty}); err != nil {
		t.Fatalf("bulk insert with NULLs: %v", err)
	}
	back, ok, err := mdwide.New().Where(mdwide.ID.Eq(empty.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("read back the empty row: %v ok=%v", err, ok)
	}
	if back.OptText.Valid || back.OptBig.Valid || back.OptDay.Valid {
		t.Errorf("an unset nullable came back valid: %+v", back)
	}
}

// The JSON predicates. PostgreSQL spells them with operators — @>, <@, ?| and
// ?& — and MySQL has none of those: containment is a function, and a key test
// becomes set overlap or set containment over JSON_KEYS. One bound value each
// way, so the statement's shape does not depend on how many keys were asked
// for.
func TestJSONPredicates(t *testing.T) {
	ctx := context.Background()
	rows := []mdwide.Row{
		{ID: id(0xe1, 1), Text: "ab", Doc: storm.JSON("{\"a\": 1, \"b\": 2}")},
		{ID: id(0xe1, 2), Text: "c", Doc: storm.JSON("{\"c\": 3}")},
	}
	for i := range rows {
		if err := mdwide.Insert(ctx, ex, &rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = ex.Exec(ctx, "DELETE FROM md_wides", nil) })

	count := func(name string, p mdwide.Pred) int64 {
		t.Helper()
		n, err := mdwide.New().Where(p).Count(ctx, ex)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return n
	}
	if n := count("contains", mdwide.Doc.Contains(storm.JSON("{\"a\": 1}"))); n != 1 {
		t.Errorf("Contains matched %d rows, want 1", n)
	}
	// Any of these keys: the second row has none of them.
	if n := count("any key", mdwide.Doc.HasAnyKey("a", "z")); n != 1 {
		t.Errorf("HasAnyKey matched %d rows, want 1", n)
	}
	// All of them, so a document with only some must NOT match — the case that
	// tells overlap from containment.
	if n := count("all keys", mdwide.Doc.HasAllKeys("a", "b")); n != 1 {
		t.Errorf("HasAllKeys matched %d rows, want 1", n)
	}
	if n := count("all keys partial", mdwide.Doc.HasAllKeys("a", "z")); n != 0 {
		t.Errorf("HasAllKeys matched %d rows for a key that is not there, want 0", n)
	}
}
`
