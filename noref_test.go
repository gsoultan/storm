package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
)

type nfInstance struct {
	storm.Model
	Name string
}

// nfHold refers to an instance it must not wait on: the reference is typed,
// and the database is told nothing about it.
type nfHold struct {
	storm.Model
	Reason   string
	Instance nfInstance
	PlacedBy *nfInstance
}

func (h *nfHold) Schema(t *storm.Table) {
	t.Col(&h.Instance).AcknowledgeNoFK("placing a hold must not wait on the instance's row")
}

func TestAReferenceCanLeaveItsForeignKeyOut(t *testing.T) {
	s, err := storm.Build(&nfInstance{}, &nfHold{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("nf_holds")
	col := tbl.Column("instance_id")
	if col == nil || col.Type.Name != s.Table("nf_instances").Column("id").Type.Name || !col.NotNull {
		t.Fatalf("instance_id is %+v; want the target key's type, not null", col)
	}
	var keys []string
	for _, fk := range tbl.ForeignKeys {
		keys = append(keys, fk.Columns[0])
	}
	if len(keys) != 1 || keys[0] != "placed_by_id" {
		t.Fatalf("foreign keys on %v; want only placed_by_id", keys)
	}
	var indexed bool
	for _, ix := range tbl.Indexes {
		if len(ix.Columns) == 1 && ix.Columns[0].Name == "instance_id" {
			indexed = true
		}
	}
	if !indexed {
		t.Error("instance_id lost its index with its foreign key")
	}
	var rel bool
	for _, r := range tbl.Relations {
		if r.Field == "Instance" {
			rel = true
			if !r.Owner || r.Column != "instance_id" || !strings.Contains(r.NoFK, "must not wait") {
				t.Errorf("relation %+v; want an owned instance_id relation carrying the reason", r)
			}
		}
		if r.Field == "PlacedBy" && r.NoFK != "" {
			t.Errorf("PlacedBy carries a reason it was not given: %q", r.NoFK)
		}
	}
	if !rel {
		t.Error("the Instance relation is gone; the reference must still load and join")
	}

	ddl := pgddl.Create(s)
	if strings.Count(ddl, "REFERENCES") != 1 || strings.Contains(ddl, `FOREIGN KEY ("instance_id")`) {
		t.Errorf("the DDL constrains instance_id:\n%s", ddl)
	}
}

type nfPlain struct {
	storm.Model
	Name string
}

func (p *nfPlain) Schema(t *storm.Table) {
	t.Col(&p.Name).AcknowledgeNoFK("there is nothing to point at")
}

type nfNoReason struct {
	storm.Model
	Instance nfInstance
}

func (h *nfNoReason) Schema(t *storm.Table) { t.Col(&h.Instance).AcknowledgeNoFK("  ") }

type nfBoth struct {
	storm.Model
	Instance nfInstance
}

func (h *nfBoth) Schema(t *storm.Table) {
	t.Col(&h.Instance).OnDelete(storm.Cascade).AcknowledgeNoFK("both at once")
}

func TestAcknowledgeNoFKIsRefusedWhereItMeansNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		models []any
		want   []string
	}{
		"a plain column":       {[]any{&nfPlain{}}, []string{"nf_plains.name", "no foreign key to leave out"}},
		"no reason":            {[]any{&nfInstance{}, &nfNoReason{}}, []string{"needs a reason"}},
		"a referential action": {[]any{&nfInstance{}, &nfBoth{}}, []string{"nf_boths.instance_id", "keep one or the other"}},
	} {
		_, err := storm.Build(tc.models...)
		if err == nil {
			t.Errorf("%s: built", name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error does not say %q:\n%v", name, w, err)
			}
		}
	}
}
