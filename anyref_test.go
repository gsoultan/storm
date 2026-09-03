package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
)

type arAudit struct {
	storm.Model
	Action  string
	Subject storm.AnyRef
}

type arAuditOK struct {
	storm.Model
	Action  string
	Subject storm.AnyRef
}

func (a *arAuditOK) Schema(t *storm.Table) {
	t.Col(&a.Subject).AcknowledgeNoFK("audit rows outlive their subjects by design")
}

func TestAnyRefMustBeAcknowledged(t *testing.T) {
	_, err := storm.Build(&arAudit{})
	if err == nil {
		t.Fatal("an unacknowledged AnyRef built; giving up integrity must be explicit")
	}
	for _, want := range []string{"referential integrity", "AcknowledgeNoFK", "OneOf", "Subject"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestAnyRefColumnsAndIndex(t *testing.T) {
	s, err := storm.Build(&arAuditOK{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("ar_audit_oks")
	if tbl == nil {
		t.Fatal("no table")
	}
	if len(tbl.AnyRefs) != 1 {
		t.Fatalf("got %d AnyRefs, want 1", len(tbl.AnyRefs))
	}
	ar := tbl.AnyRefs[0]
	if ar.TypeColumn != "subject_type" || ar.IDColumn != "subject_id" {
		t.Errorf("columns are %q/%q", ar.TypeColumn, ar.IDColumn)
	}
	if ar.Reason == "" {
		t.Error("the acknowledgement did not reach the schema; a diff would not show it")
	}
	for _, n := range []string{"subject_type", "subject_id"} {
		if tbl.Column(n) == nil {
			t.Errorf("no %s column", n)
		}
	}
	// The pair is indexed together: a lookup names the type first, and neither
	// column alone is selective.
	var found bool
	for _, ix := range tbl.Indexes {
		if len(ix.Columns) == 2 && ix.Columns[0].Name == "subject_type" && ix.Columns[1].Name == "subject_id" {
			found = true
		}
	}
	if !found {
		t.Error("no composite (subject_type, subject_id) index")
	}
	// No foreign key: there is nothing to point at, and a generated one would
	// be a lie the database then enforces against the wrong table.
	for _, fk := range tbl.ForeignKeys {
		for _, c := range fk.Columns {
			if c == "subject_id" || c == "subject_type" {
				t.Errorf("a foreign key was generated on %s -> %s; AnyRef cannot have one",
					c, fk.RefTable)
			}
		}
	}
}

// The DDL is the point of the whole feature: two ordinary columns, a composite
// index, and NOT ONE foreign key. A generated FK here would be a lie the
// database then enforces against whichever table happened to be named first.
func TestAnyRefDDL(t *testing.T) {
	s, err := storm.Build(&arAuditOK{})
	if err != nil {
		t.Fatal(err)
	}
	ddl := pgddl.Create(s)
	for _, want := range []string{
		`"subject_type" text NOT NULL`,
		`"subject_id" uuid NOT NULL`,
		`("subject_type", "subject_id")`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL is missing %s:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "subject_id\" uuid NOT NULL REFERENCES") ||
		strings.Contains(ddl, "FOREIGN KEY (\"subject_id\")") {
		t.Errorf("a foreign key was generated for an AnyRef:\n%s", ddl)
	}
}

// AcknowledgeNoFK on an ordinary reference is a mistake worth naming: it reads
// as though integrity were being waived somewhere it is not.
func TestAcknowledgeNoFKOnlyAppliesToAnyRef(t *testing.T) {
	_, err := storm.Build(&arWrongAck{})
	if err == nil {
		t.Fatal("AcknowledgeNoFK was accepted on an ordinary column")
	}
	if !strings.Contains(err.Error(), "only meaningful on a storm.AnyRef") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// An empty reason is the same mistake as no reason: the diff shows nothing.
func TestAcknowledgeNoFKNeedsAReason(t *testing.T) {
	_, err := storm.Build(&arBlankAck{})
	if err == nil {
		t.Fatal("a blank acknowledgement was accepted")
	}
	if !strings.Contains(err.Error(), "needs a reason") {
		t.Errorf("unhelpful error: %v", err)
	}
}

type arWrongAck struct {
	storm.Model
	Note string
}

func (a *arWrongAck) Schema(t *storm.Table) {
	t.Col(&a.Note).AcknowledgeNoFK("this is not an AnyRef")
}

type arBlankAck struct {
	storm.Model
	Subject storm.AnyRef
}

func (a *arBlankAck) Schema(t *storm.Table) {
	t.Col(&a.Subject).AcknowledgeNoFK("   ")
}
