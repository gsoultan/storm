package storm_test

import (
	"testing"

	"github.com/gsoultan/storm"
)

type rnPerson struct {
	storm.Model
	Name   string
	Visits []rnVisit
}

func (p *rnPerson) Schema(t *storm.Table) { t.Name("people") }

type rnVisit struct {
	storm.Model
	Person rnPerson
}

// A foreign key is built before the Schema methods run, so it used to point
// at the target's DERIVED name: rn_people's t.Name("people") left the visit's
// key referencing a table that does not exist, and the has-many back from the
// person found no key at all and failed the build.
func TestForeignKeyFollowsTargetRename(t *testing.T) {
	s, err := storm.Build(&rnPerson{}, &rnVisit{})
	if err != nil {
		t.Fatal(err)
	}
	v := s.Table("rn_visits")
	if v == nil || len(v.ForeignKeys) != 1 {
		t.Fatalf("want rn_visits with one foreign key, got %+v", v)
	}
	if got := v.ForeignKeys[0].RefTable; got != "people" {
		t.Errorf("foreign key references %q, want the renamed table %q", got, "people")
	}
	for _, r := range v.Relations {
		if r.Field == "Person" && r.Target != "people" {
			t.Errorf("relation Person targets %q, want %q", r.Target, "people")
		}
	}
	p := s.Table("people")
	if p == nil {
		t.Fatal("people missing")
	}
	var found bool
	for _, r := range p.Relations {
		if r.Field == "Visits" && r.ToMany {
			found = true
		}
	}
	if !found {
		t.Error("people has no has-many Visits relation")
	}
}
