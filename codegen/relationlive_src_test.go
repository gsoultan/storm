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
				ID: id(a, byte(0x40+i)), Title: "post", AuthorID: author.ID,
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
`
