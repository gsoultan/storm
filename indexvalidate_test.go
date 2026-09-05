package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/schema"
)

// One fixture per rule. Each is standalone, for the reason the aggregate
// fixtures are.

type ixTwoNames struct {
	storm.Model
	Slug string
}

func (m *ixTwoNames) Schema(t *storm.Table) {
	t.Index(&m.Slug)
	t.Index(&m.Slug).Using(storm.Hash) // same generated name
}

type ixUnknownMethod struct {
	storm.Model
	Slug string
}

func (m *ixUnknownMethod) Schema(t *storm.Table) { t.Index(&m.Slug).Using("gits") }

type ixUniqueGin struct {
	storm.Model
	Tags []string
}

func (m *ixUniqueGin) Schema(t *storm.Table) { t.Index(&m.Tags).Using(storm.GIN).Unique() }

type ixNNDWithoutUnique struct {
	storm.Model
	Ref *string
}

func (m *ixNNDWithoutUnique) Schema(t *storm.Table) { t.Index(&m.Ref).NullsNotDistinct() }

type ixHashTwoKeys struct {
	storm.Model
	A, B string
}

func (m *ixHashTwoKeys) Schema(t *storm.Table) { t.Index(&m.A, &m.B).Using(storm.Hash) }

type ixIncludeOnGin struct {
	storm.Model
	Tags []string
	Name string
}

func (m *ixIncludeOnGin) Schema(t *storm.Table) { t.Index(&m.Tags).Using(storm.GIN).Include(&m.Name) }

type ixIncludeIsKey struct {
	storm.Model
	Slug string
}

func (m *ixIncludeIsKey) Schema(t *storm.Table) { t.Index(&m.Slug).Include(&m.Slug) }

type ixBadParam struct {
	storm.Model
	Slug string
}

func (m *ixBadParam) Schema(t *storm.Table) { t.Index(&m.Slug).With("fill_factor", "70") }

type ixParamWrongMethod struct {
	storm.Model
	Slug string
}

func (m *ixParamWrongMethod) Schema(t *storm.Table) { t.Index(&m.Slug).With("pages_per_range", "32") }

type ixDescNullsFirst struct {
	storm.Model
	Score *int32
}

func (m *ixDescNullsFirst) Schema(t *storm.Table) {
	t.Index(storm.NullsFirst(storm.Desc(&m.Score)))
}

type ixAscNullsLast struct {
	storm.Model
	Score *int32
}

func (m *ixAscNullsLast) Schema(t *storm.Table) { t.Index(storm.NullsLast(&m.Score)) }

type ixOpClassWrongMethod struct {
	storm.Model
	Prefs map[string]any
}

func (m *ixOpClassWrongMethod) Schema(t *storm.Table) {
	t.Index(storm.OpClass(&m.Prefs, "jsonb_path_ops")) // btree
}

type ixPrefixOnInt struct {
	storm.Model
	N int32
}

func (m *ixPrefixOnInt) Schema(t *storm.Table) { t.Index(storm.Prefix(&m.N, 4)) }

type ixFullTextOnInt struct {
	storm.Model
	N int32
}

func (m *ixFullTextOnInt) Schema(t *storm.Table) { t.Index(&m.N).Using(storm.FullText) }

type ixColumnTwice struct {
	storm.Model
	Slug string
}

func (m *ixColumnTwice) Schema(t *storm.Table) { t.Index(&m.Slug, storm.Desc(&m.Slug)) }

// The rules that keep a migration converging. Most of these the database
// would accept — and then print back without the clause, so the next diff
// would recreate the index forever.
func TestIndexDeclarationsAreCheckedAtBuildTime(t *testing.T) {
	cases := []struct {
		name  string
		model any
		want  []string
	}{
		{"two indexes generate one name", &ixTwoNames{}, []string{"twice", "Named"}},
		{"unknown method", &ixUnknownMethod{}, []string{`"gits"`, "btree", "gin"}},
		{"unique gin", &ixUniqueGin{}, []string{"UNIQUE", "btree"}},
		{"nulls not distinct without unique", &ixNNDWithoutUnique{}, []string{"NULLS NOT DISTINCT", "UNIQUE"}},
		{"hash over two keys", &ixHashTwoKeys{}, []string{"hash", "exactly one"}},
		{"include on gin", &ixIncludeOnGin{}, []string{"INCLUDE", "gin"}},
		{"include column is a key", &ixIncludeIsKey{}, []string{"both a key and an INCLUDE"}},
		{"unknown storage parameter", &ixBadParam{}, []string{`"fill_factor"`, "fillfactor"}},
		{"parameter on the wrong method", &ixParamWrongMethod{}, []string{"pages_per_range", "brin", "btree"}},
		{"desc nulls first is the default", &ixDescNullsFirst{}, []string{"DESC NULLS FIRST", "recreate"}},
		{"asc nulls last is the default", &ixAscNullsLast{}, []string{"ASC NULLS LAST", "recreate"}},
		{"opclass on the wrong method", &ixOpClassWrongMethod{}, []string{"jsonb_path_ops", "gin", "Using(storm.GIN)"}},
		{"prefix on an integer", &ixPrefixOnInt{}, []string{"prefix", "text or bytes"}},
		{"fulltext over an integer", &ixFullTextOnInt{}, []string{"FULLTEXT", "not a text column"}},
		{"column named twice", &ixColumnTwice{}, []string{"twice"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := storm.Build(c.model)
			if err == nil {
				t.Fatal("accepted; the database would refuse it, or forget it")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

// The accepted shape: every clause at once, composed in any order.
type ixKitchenSink struct {
	storm.Model
	Slug  string
	Title string
	Body  string
	Score *int32
	Ref   *string
}

func (m *ixKitchenSink) Schema(t *storm.Table) {
	t.Index(storm.OpClass(storm.Collate(storm.Lower(&m.Slug), "C"), "text_pattern_ops"),
		storm.NullsFirst(&m.Score)).
		Include(&m.Title).With("fillfactor", "70").Where("score > 0").Named("ix_cover")
	t.Index(storm.OpClass(&m.Body, "gin_trgm_ops")).Using(storm.GIN).With("fastupdate", "off")
	t.Index(&m.Ref).Unique().NullsNotDistinct()
	t.Index(storm.NullsLast(storm.Desc(&m.Score))).Named("ix_score_desc")
	t.Index(storm.IndexExpr(&m.Score, "(%s + 1)")).Named("ix_score_next")
	t.Index(storm.Prefix(&m.Body, 191)).Named("ix_body_prefix")
	t.Index(&m.Title).Using(storm.FullText).Named("ix_title_ft")
}

func TestValidIndexesBuild(t *testing.T) {
	s, err := storm.Build(&ixKitchenSink{})
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Tables[0]
	if len(tb.Indexes) != 7 {
		t.Fatalf("want 7 indexes, got %d", len(tb.Indexes))
	}
	// Normalisation orders the indexes by name, so look the covering one up.
	var first *schema.Index
	for _, ix := range tb.Indexes {
		if ix.Name == "ix_cover" {
			first = ix
		}
	}
	if first == nil {
		t.Fatal("the covering index is missing")
	}
	k := first.Columns[0]
	if !k.Expr || k.Name != "lower(slug)" || k.Collate != "C" || k.OpClass != "text_pattern_ops" {
		t.Errorf("modifiers did not compose: %+v", k)
	}
	if !first.Columns[1].NullsFirst || first.Columns[1].Desc {
		t.Errorf("NullsFirst on an ascending key was lost: %+v", first.Columns[1])
	}
	if len(first.Include) != 1 || first.Include[0] != "title" {
		t.Errorf("INCLUDE was lost: %v", first.Include)
	}
	// The redundant parentheses a caller wrapped the expression in are gone,
	// so the key reads back from the server as it was declared.
	for _, ix := range tb.Indexes {
		if ix.Name == "ix_score_next" && ix.Columns[0].Name != "score + 1" {
			t.Errorf("expression key kept its outer parentheses: %q", ix.Columns[0].Name)
		}
	}
}
