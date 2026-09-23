package orabench

// Introspection against a real server, which is the only way to test it: every
// query here reads a catalogue, and a catalogue is not something a fake can be
// honest about.
//
// The shape is a ROUND TRIP — model → DDL → server → model — because that is
// the property `storm import` actually promises. A field-by-field assertion on
// one table would pass while the import silently halved every string width,
// which is the defect M10 shipped against SQL Server's max_length.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/runtime/sqldrv"
	"github.com/gsoultan/storm/schema"
	oraintro "github.com/gsoultan/storm/schema/oracle"
)

type impOrg struct {
	storm.Model
	Name    string
	Seats   int32
	Balance storm.Decimal
	Active  bool
	Opened  time.Time
	Blob    []byte
	Doc     storm.JSON
	Upper   string
}

func (o *impOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Balance).Numeric(18, 4)
	t.Col(&o.Opened).Date()
	t.Col(&o.Upper).Generated(`UPPER("name")`)
	t.Unique(&o.Name)
	t.Index(&o.Seats)
}

type impMember struct {
	storm.Model
	Email     string
	Rank      int64
	DeletedAt *time.Time
	Org       impOrg
}

func (m *impMember) Schema(t *storm.Table) {
	t.Col(&m.Email).Size(255)
	t.SoftDelete(&m.DeletedAt)
	t.Index(&m.Email).Unique().Where(`"deleted_at" IS NULL`)
}

func TestIntrospectionRoundTrip(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	want, err := storm.Build(&impOrg{}, &impMember{})
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := oraddl.Statements(want)
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{"imp_members", "imp_orgs"}
	dropAll := func() {
		for _, tb := range tables {
			drop(db, "TABLE", `"`+tb+`" CASCADE CONSTRAINTS PURGE`)
		}
	}
	dropAll()
	t.Cleanup(dropAll)
	for _, st := range stmts {
		if _, err := db.Exec(st); err != nil {
			t.Fatalf("applying:\n%s\n%v", st, err)
		}
	}

	got, err := oraintro.Introspect(ctx, sqldrv.New(db), "")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	orgs := tableOf(t, got, "imp_orgs")
	members := tableOf(t, got, "imp_members")

	// THE WIDTH. char_length is characters and data_length is bytes, and a
	// VARCHAR2(200 CHAR) reports 800 of the latter in a multi-byte character
	// set — so a reader that picked the wrong one quadruples every imported
	// width. M10 shipped that defect against max_length.
	assertCol2(t, orgs, "name", schema.TypeVarchar, 200)
	assertCol2(t, orgs, "seats", schema.TypeInt4, 0)
	assertCol2(t, orgs, "balance", schema.TypeNumeric, 0)
	assertCol2(t, orgs, "active", schema.TypeBool, 0)
	assertCol2(t, orgs, "opened", schema.TypeDate, 0)
	assertCol2(t, orgs, "blob", schema.TypeBytea, 0)
	assertCol2(t, orgs, "doc", schema.TypeJSONB, 0)
	// RAW(16) is the uuid and nothing else is.
	assertCol2(t, orgs, "id", schema.TypeUUID, 0)

	if c := orgs.Column("balance"); c == nil || c.Type.Precision != 18 || c.Type.Scale != 4 {
		t.Errorf("the decimal's precision did not survive: %+v", c)
	}
	// A VIRTUAL column's expression is stored where a default is, so reading
	// it as a default would give a model that inserts the expression as text.
	if c := orgs.Column("upper"); c == nil || c.Generated == "" {
		t.Errorf("the generated column came back as %+v", c)
	} else if c.Default != "" {
		t.Errorf("a virtual column's expression was read as a DEFAULT: %q", c.Default)
	}

	// NOT NULL is a CHECK constraint in Oracle's catalogue, with a
	// system-generated name. Importing those would give every model a
	// duplicate of what the column already says.
	for _, ck := range orgs.Checks {
		if strings.Contains(strings.ToUpper(ck.Expr), "IS NOT NULL") {
			t.Errorf("a system NOT NULL check was imported as a model check: %s", ck.Expr)
		}
	}

	// The primary key, and the unique CONSTRAINT — which must not also appear
	// as an index, or every diff would propose to drop one.
	if len(orgs.PrimaryKey) != 1 || orgs.PrimaryKey[0] != "id" {
		t.Errorf("primary key came back as %v", orgs.PrimaryKey)
	}
	if len(orgs.Uniques) != 1 || len(orgs.Uniques[0].Columns) != 1 ||
		orgs.Uniques[0].Columns[0] != "name" {
		t.Errorf("unique constraint came back as %+v", orgs.Uniques)
	}
	for _, ix := range orgs.Indexes {
		if ix.Unique && len(ix.Columns) == 1 && ix.Columns[0].Name == "name" {
			t.Error("the unique CONSTRAINT also came back as an index; a diff would " +
				"propose to drop one of them forever")
		}
	}

	// The partial unique, which is a function-based index on a CASE here. Its
	// key is an EXPRESSION, and the expression's text is not folded — `'paid'`
	// and `'PAID'` are different values.
	var partial *schema.Index
	for _, ix := range members.Indexes {
		if ix.Unique {
			partial = ix
		}
	}
	if partial == nil {
		t.Fatal("the live-scoped unique did not come back")
	}
	if len(partial.Columns) != 1 || !partial.Columns[0].Expr {
		t.Fatalf("the partial unique's key is not an expression: %+v", partial.Columns)
	}
	if !strings.Contains(strings.ToUpper(partial.Columns[0].Name), "CASE") {
		t.Errorf("the CASE did not survive: %q", partial.Columns[0].Name)
	}

	// The foreign key, and NO ON UPDATE — Oracle has no such clause, so there
	// is nothing in the catalogue to read.
	if len(members.ForeignKeys) != 1 {
		t.Fatalf("foreign keys came back as %+v", members.ForeignKeys)
	}
	fk := members.ForeignKeys[0]
	if fk.RefTable != "imp_orgs" || len(fk.Columns) != 1 || fk.Columns[0] != "org_id" {
		t.Errorf("the foreign key came back as %+v", fk)
	}
	if fk.OnUpdate != schema.NoAction {
		t.Errorf("an ON UPDATE appeared from a dialect that has no such clause: %q", fk.OnUpdate)
	}
}

// And the case rule, which is the thing no other introspector needs: a table
// created UNQUOTED shouts in the catalogue, and a model generated from it
// verbatim would declare Go fields from names nobody wrote.
func TestAnUnquotedTableComesBackLowercased(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	drop(db, "TABLE", "shouty CASCADE CONSTRAINTS PURGE")
	mustExec(t, db, `CREATE TABLE shouty (id NUMBER(19) PRIMARY KEY, some_name VARCHAR2(40 CHAR))`)
	t.Cleanup(func() { drop(db, "TABLE", "shouty CASCADE CONSTRAINTS PURGE") })

	got, err := oraintro.Introspect(ctx, sqldrv.New(db), "")
	if err != nil {
		t.Fatal(err)
	}
	tb := got.Table("shouty")
	if tb == nil {
		var names []string
		for _, x := range got.Tables {
			names = append(names, x.Name)
		}
		t.Fatalf("SHOUTY did not come back as `shouty`; got %v", names)
	}
	if tb.Column("some_name") == nil {
		t.Error("the column did not come back lowercased either")
	}
}

func tableOf(t *testing.T, s *schema.Schema, name string) *schema.Table {
	t.Helper()
	tb := s.Table(name)
	if tb == nil {
		var names []string
		for _, x := range s.Tables {
			names = append(names, x.Name)
		}
		t.Fatalf("%s is not in the imported schema; got %v", name, names)
	}
	return tb
}

func assertCol2(t *testing.T, tb *schema.Table, name, wantType string, wantSize int) {
	t.Helper()
	c := tb.Column(name)
	if c == nil {
		t.Errorf("%s.%s is missing", tb.Name, name)
		return
	}
	if c.Type.Name != wantType {
		t.Errorf("%s.%s is %s, want %s", tb.Name, name, c.Type.Name, wantType)
	}
	if wantSize != 0 && c.Type.Size != wantSize {
		t.Errorf("%s.%s has size %d, want %d", tb.Name, name, c.Type.Size, wantSize)
	}
}

var _ = sql.ErrNoRows
