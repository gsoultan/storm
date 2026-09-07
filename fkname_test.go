package storm_test

import (
	"testing"

	"github.com/gsoultan/storm"
)

type fkParent struct {
	storm.Model
	Name string
}

type fkChild struct {
	storm.Model
	Parent fkParent
}

func (c *fkChild) Schema(t *storm.Table) {
	t.Col(&c.Parent).ConstraintName("fk_childs_parent_id_legacy")
}

// storm derives fk_<table>_<column>; PostgreSQL's own default is
// <table>_<column>_fkey. Importing an existing database therefore produced a
// model whose first migration DROPped and re-ADDed every foreign key — a lock
// on a large table, and a rename of a constraint an application may be
// matching on, to gain nothing.
func TestForeignKeyNameCanBePinned(t *testing.T) {
	s, err := storm.Build(&fkParent{}, &fkChild{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("fk_childs")
	if tbl == nil {
		t.Fatalf("fk_childs missing; tables: %v", func() []string {
			var n []string
			for _, x := range s.Tables {
				n = append(n, x.Name)
			}
			return n
		}())
	}
	if len(tbl.ForeignKeys) != 1 {
		t.Fatalf("want 1 foreign key, got %d", len(tbl.ForeignKeys))
	}
	if got := tbl.ForeignKeys[0].Name; got != "fk_childs_parent_id_legacy" {
		t.Errorf("foreign key name = %q, want the pinned one", got)
	}
}
