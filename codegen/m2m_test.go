package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

type lkArticle struct {
	storm.Model
	Title  string
	Topics []lkTopic
}

type lkTopic struct {
	storm.Model
	Name     string
	Articles []lkArticle
}

func (a *lkArticle) Plans(p *storm.Plans) {
	p.Named("Full").With(&a.Topics)
}

// A slice on both sides is a many-to-many, and the join table is storm's to
// generate — an adopter who has to declare it has been given a chore, not a
// feature.
func TestManyToManyGeneratesTheJoinTable(t *testing.T) {
	s, err := storm.Build(&lkArticle{}, &lkTopic{})
	if err != nil {
		t.Fatal(err)
	}
	link := s.Table("lk_article_lk_topics")
	if link == nil {
		t.Fatal("no join table was generated")
	}
	if !link.Generated {
		t.Error("the join table is not marked Generated; storm diff cannot say where it came from")
	}
	if len(link.PrimaryKey) != 2 {
		t.Errorf("primary key is %v, want both columns", link.PrimaryKey)
	}
	if len(link.ForeignKeys) != 2 {
		t.Fatalf("got %d foreign keys, want 2", len(link.ForeignKeys))
	}
	for _, fk := range link.ForeignKeys {
		if fk.OnDelete != "CASCADE" {
			t.Errorf("%v on delete %q — an association whose end is gone is not a row worth keeping",
				fk.Columns, fk.OnDelete)
		}
	}
	// The composite PK indexes (a, b); the reverse direction needs its own.
	if len(link.Indexes) != 1 || link.Indexes[0].Columns[0].Name != "lk_topic_id" {
		t.Errorf("no reverse index: %+v", link.Indexes)
	}

	// Both sides are wired, from one place, so they cannot disagree.
	for _, tc := range []struct{ table, field, own, far string }{
		{"lk_articles", "Topics", "lk_article_id", "lk_topic_id"},
		{"lk_topics", "Articles", "lk_topic_id", "lk_article_id"},
	} {
		var found bool
		for _, r := range s.Table(tc.table).Relations {
			if r.Field != tc.field {
				continue
			}
			found = true
			if r.Link != link.Name || r.LinkColumn != tc.own || r.LinkTargetColumn != tc.far {
				t.Errorf("%s.%s wired as (%s, %s, %s)", tc.table, tc.field, r.Link, r.LinkColumn, r.LinkTargetColumn)
			}
			if r.Column != "" {
				t.Errorf("%s.%s carries a column; neither side of a many-to-many does", tc.table, tc.field)
			}
		}
		if !found {
			t.Errorf("%s has no %s relation", tc.table, tc.field)
		}
	}
}

// A many-to-many member costs TWO queries, and lint must say so. A budget
// computed from the wrong number is a check somebody trusts and should not.
func TestManyToManyPlanCostIsTwo(t *testing.T) {
	s, err := storm.Build(&lkArticle{}, &lkTopic{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range s.Tables {
		names = append(names, tb.Name)
	}
	costs, err := codegen.PlanCosts(s, names)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range costs {
		if !strings.HasSuffix(c.Name, "Full") {
			continue
		}
		found = true
		// 1 for the articles, 2 for the link hop.
		if c.RoundTrips != 3 {
			t.Errorf("%s costs %d round trips, want 3 (%s)", c.Name, c.RoundTrips, c.Chain)
		}
		if !strings.Contains(c.Chain, "lk_article_lk_topics") {
			t.Errorf("the chain does not name the join table: %s", c.Chain)
		}
	}
	if !found {
		t.Fatal("the named plan over a many-to-many was not costed")
	}
}

// A named plan must load through a join table as readily as the automatic
// per-relation tier does.
func TestManyToManyNamedPlanGenerates(t *testing.T) {
	s, err := storm.Build(&lkArticle{}, &lkTopic{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: "gen", Import: "github.com/gsoultan/storm",
		Package: "gen", PackageImport: "example.com/x/gen",
	})
	if err != nil {
		t.Fatalf("codegen: %v", err)
	}
	var ctx string
	for name, src := range files {
		if !strings.Contains(name, "/") {
			ctx = string(src)
		}
	}
	if ctx == "" {
		t.Fatal("no context file")
	}
	for _, want := range []string{
		"func LkArticleFull()",       // the named plan
		"func LkArticleWithTopics()", // the automatic tier
		"lkarticlelktopic.",          // the join table's package
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context is missing %q", want)
		}
	}
	// ChildTop cannot work through a join table, so it must not be offered:
	// a method that cannot work is better absent than failing at run time.
	if strings.Contains(ctx, "func (p LkArticleWithTopicsQuery) ChildTop(") {
		t.Error("ChildTop was generated for a many-to-many; it has no lowering through a join table")
	}
}
