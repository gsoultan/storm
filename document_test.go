package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
)

// OnDocument is how a reference says what it is on a document target, and it
// must change nothing on a SQL one.
type docOrg struct {
	storm.Model
	Name string
}

type docUser struct {
	storm.Model
	Org docOrg
}

func (u *docUser) Schema(t *storm.Table) {
	t.Col(&u.Org).OnDelete(storm.Cascade).OnDocument(storm.Reference())
}

func TestOnDocumentRecordsTheFormAndKeepsTheForeignKey(t *testing.T) {
	s, err := storm.Build(&docOrg{}, &docUser{})
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Table("doc_users")
	if tb == nil || len(tb.ForeignKeys) != 1 || tb.ForeignKeys[0].OnDelete != "CASCADE" {
		t.Fatalf("the foreign key must stay exactly as declared: %+v", tb)
	}
	for _, r := range tb.Relations {
		if r.Owner && r.Document != string(storm.Reference()) {
			t.Errorf("owning relation %s carries %q", r.Field, r.Document)
		}
	}
}

type docWrong struct {
	storm.Model
	Name string
}

func (d *docWrong) Schema(t *storm.Table) { t.Col(&d.Name).OnDocument(storm.Reference()) }

type docUnknown struct {
	storm.Model
	Org docOrg
}

func (d *docUnknown) Schema(t *storm.Table) { t.Col(&d.Org).OnDocument("embed") }

func TestOnDocumentIsRefusedWhereItMeansNothing(t *testing.T) {
	if _, err := storm.Build(&docWrong{}); err == nil || !strings.Contains(err.Error(), "only meaningful on a reference") {
		t.Errorf("OnDocument on a plain column: %v", err)
	}
	if _, err := storm.Build(&docOrg{}, &docUnknown{}); err == nil || !strings.Contains(err.Error(), "storm.Reference()") {
		t.Errorf("an unknown form: %v", err)
	}
}
