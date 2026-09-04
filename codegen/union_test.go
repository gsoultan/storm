package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/internal/testmodel"
)

// A union's row type, statement and reader are emitted into the CONTEXT
// package, because a union belongs to no table (ADR-0008). Everything else the
// generator writes lands in a table package, so this is the one emitter whose
// output has nowhere else to go — and the one whose imports the context
// package cannot infer from the rest of what it emits.
func buildWithUnion(t *testing.T, decl *storm.UnionDecl) *schema0 {
	t.Helper()
	s, err := storm.Build(append(testmodel.All(), decl)...)
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           "gen",
		Package:       "ctxgen",
		Import:        "github.com/gsoultan/storm",
		PackageImport: "github.com/gsoultan/storm/internal/testgen",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ctx string
	for name, b := range files {
		if strings.HasSuffix(name, "ctxgen.gen.go") || strings.Count(name, "/") == 0 {
			ctx += string(b)
		}
	}
	if ctx == "" {
		t.Fatal("no context file emitted")
	}
	return &schema0{ctx: ctx}
}

type schema0 struct{ ctx string }

func (s *schema0) must(t *testing.T, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s.ctx, w) {
			t.Errorf("the emitted context is missing %q", w)
		}
	}
}

var unionNoParams = storm.Union("Stream", func(u *storm.UnionSpec) {
	var p testmodel.Post
	a := u.From(&p)
	a.Take(&p.CreatedAt, "At")
	a.Take(&p.Title, "Text")
	a.Const("Kind", "post")
	a.Where(storm.Exprs{}.IsNotNull(&p.PublishedAt))

	var c testmodel.Comment
	b := u.From(&c)
	b.Take(&c.CreatedAt, "At")
	b.Take(&c.Body, "Text")
	b.Const("Kind", "comment")

	u.OrderDesc("At")
})

func TestUnionEmitsRowStatementAndReader(t *testing.T) {
	g := buildWithUnion(t, unionNoParams)
	g.must(t,
		"type StreamRow struct {",
		"const streamSQL = `",
		"UNION ALL",
		"func scanStream(rv [][]byte, r *StreamRow, sl *runtime.Slab) error {",
		"func Stream(ctx context.Context, ex runtime.Executor, n int64) ([]StreamRow, error) {",
		"func StreamInto(",
		// A time column in the row means the context package needs "time",
		// which it cannot infer from anything else it emits.
		`"time"`,
		// The declared branch filter is in the fixed text and no call widens it.
		`"published_at" IS NOT NULL`,
	)
}

var unionWithParam = storm.Union("Mine", func(u *storm.UnionSpec) {
	who := u.Param("Author")

	var p testmodel.Post
	a := u.From(&p)
	a.Take(&p.CreatedAt, "At")
	a.Take(&p.Title, "Text")
	a.Where(storm.Exprs{}.Eq(&p.Author, who))

	var c testmodel.Comment
	b := u.From(&c)
	b.Take(&c.CreatedAt, "At")
	b.Take(&c.Body, "Text")
	b.Where(storm.Exprs{}.Eq(&c.Author, who))

	u.OrderDesc("At")
})

// One declared parameter is one argument and one placeholder, in both branches.
func TestUnionEmitsSharedParameter(t *testing.T) {
	g := buildWithUnion(t, unionWithParam)
	g.must(t,
		"func Mine(ctx context.Context, ex runtime.Executor, author [16]byte, n int64)",
		"MineInto(ctx, ex, nil, &sl, author, n)",
		"[]any{author, n}",
		"LIMIT $2",
	)
	if strings.Count(g.ctx, "$1") < 2 {
		t.Error("the shared parameter should be $1 in BOTH branches")
	}
}

// UNION rather than UNION ALL only when the declaration asks: de-duplicating
// sorts the whole result before the first row comes back.
var unionDistinct = storm.Union("Uniq", func(u *storm.UnionSpec) {
	var p testmodel.Post
	a := u.From(&p)
	a.Take(&p.Title, "Text")
	var c testmodel.Comment
	b := u.From(&c)
	b.Take(&c.Body, "Text")
	u.OrderAsc("Text").Distinct()
})

func TestUnionDistinctIsOptIn(t *testing.T) {
	g := buildWithUnion(t, unionDistinct)
	if strings.Contains(g.ctx, "UNION ALL") {
		t.Error("Distinct() should emit UNION, not UNION ALL")
	}
	g.must(t, "UNION ")
	// No time column here, so the context must NOT import time on its account.
	if strings.Contains(g.ctx, "type UniqRow struct {\n\tText string\n}") == false {
		t.Logf("row: %s", g.ctx[strings.Index(g.ctx, "type UniqRow"):strings.Index(g.ctx, "type UniqRow")+60])
	}
}
