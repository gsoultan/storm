package mssql_test

// Introspection against a real server, which is the only way to test it: every
// query here reads a catalogue, and a catalogue is not something a fake can be
// honest about.
//
// The shape of the test is a ROUND TRIP — model → DDL → server → model — because
// that is the property `storm import` actually promises. A field-by-field
// assertion on one table would pass while the import silently halved every
// string width; a round trip notices.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/runtime/msdrv"
	"github.com/gsoultan/storm/schema"
	msintro "github.com/gsoultan/storm/schema/mssql"
)

type impStatus string

func (impStatus) EnumValues() []string { return []string{"new", "paid"} }

type impOrg struct {
	storm.Model
	Name    string
	Seats   int32
	Balance storm.Decimal
	Active  bool
	Opened  time.Time
	Note    *string
	Blob    []byte
	Status  impStatus
	Upper   string
}

func (o *impOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Balance).Numeric(19, 4)
	t.Col(&o.Opened).Date()
	t.Col(&o.Upper).Generated("UPPER([name])")
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
	t.Col(&m.Org).OnDelete(storm.Cascade)
	t.SoftDelete(&m.DeletedAt)
	t.Unique(&m.Email)
}

func open(t *testing.T) *msdrv.Conn {
	t.Helper()
	addr := os.Getenv("STORM_MSSQL_ADDR")
	if addr == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	c, err := msdrv.Open(context.Background(), msdrv.Config{
		Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "master", TLS: msdrv.TLSDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestIntrospectRoundTrip(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	want, err := storm.Build(&impOrg{}, &impMember{})
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := msddl.Create(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"imp_members", "imp_orgs"} {
		if _, err := c.Exec(ctx, "DROP TABLE IF EXISTS ["+tb+"]", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range strings.Split(ddl, ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			if _, err := c.Exec(ctx, s, nil); err != nil {
				t.Fatalf("applying the model's own DDL:\n  %s\n%v", s, err)
			}
		}
	}
	t.Cleanup(func() {
		for _, tb := range []string{"imp_members", "imp_orgs"} {
			_, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS ["+tb+"]", nil)
		}
	})

	got, err := msintro.Introspect(ctx, c, "dbo")
	if err != nil {
		t.Fatal(err)
	}

	orgs := tableOf(t, got, "imp_orgs")
	members := tableOf(t, got, "imp_members")

	if len(orgs.PrimaryKey) != 1 || orgs.PrimaryKey[0] != "id" {
		t.Errorf("primary key = %v", orgs.PrimaryKey)
	}

	// The widths. max_length is BYTES and an nvarchar's characters are two of
	// them, so reading it as a character count halves every imported width —
	// a model that compiles and truncates.
	if c := columnOf(t, orgs, "name"); c.Type.Name != schema.TypeVarchar || c.Type.Size != 200 {
		t.Errorf("name came back as %s(%d), want varchar(200)", c.Type.Name, c.Type.Size)
	}
	if c := columnOf(t, members, "email"); c.Type.Size != 255 {
		t.Errorf("email came back %d wide, want 255", c.Type.Size)
	}

	// Every type that crosses, back again.
	for _, tc := range []struct {
		col  string
		want string
	}{
		{"id", schema.TypeUUID},
		{"created_at", schema.TypeTimestamptz},
		{"seats", schema.TypeInt4},
		{"active", schema.TypeBool},
		{"opened", schema.TypeDate},
		{"blob", schema.TypeBytea},
	} {
		if c := columnOf(t, orgs, tc.col); c.Type.Name != tc.want {
			t.Errorf("%s came back as %s, want %s", tc.col, c.Type.Name, tc.want)
		}
	}
	if c := columnOf(t, orgs, "balance"); c.Type.Precision != 19 || c.Type.Scale != 4 {
		t.Errorf("balance came back as (%d,%d), want (19,4)", c.Type.Precision, c.Type.Scale)
	}

	// Nullability, which the model states and the catalogue reports inverted.
	if c := columnOf(t, orgs, "note"); c.NotNull {
		t.Error("a nullable column came back NOT NULL")
	}
	if c := columnOf(t, orgs, "name"); !c.NotNull {
		t.Error("a NOT NULL column came back nullable")
	}

	// The DEFAULT, with the catalogue's own parentheses peeled: it stores
	// `(newid())`, and importing that verbatim puts brackets into a model
	// nobody wrote.
	if c := columnOf(t, orgs, "id"); !strings.EqualFold(c.Default, "newid()") {
		t.Errorf("the default came back as %q, want newid()", c.Default)
	}

	// The computed column, likewise.
	if c := columnOf(t, orgs, "upper"); !strings.Contains(strings.ToLower(c.Generated), "upper") {
		t.Errorf("the computed column came back as %q", c.Generated)
	}

	// The FILTERED unique — the index MySQL cannot express — with its
	// predicate, because without it the index means something else entirely.
	var filtered *schema.Index
	for _, ix := range members.Indexes {
		if ix.Unique && ix.Where != "" {
			filtered = ix
		}
	}
	if filtered == nil {
		t.Fatalf("the live-scoped unique did not come back: %+v", members.Indexes)
	}
	if !strings.Contains(strings.ToLower(filtered.Where), "deleted_at") {
		t.Errorf("the filter came back as %q", filtered.Where)
	}

	// The foreign key, its columns and its action.
	if len(members.ForeignKeys) != 1 {
		t.Fatalf("%d foreign key(s), want 1", len(members.ForeignKeys))
	}
	fk := members.ForeignKeys[0]
	if fk.RefTable != "imp_orgs" || len(fk.Columns) != 1 || fk.Columns[0] != "org_id" {
		t.Errorf("foreign key = %+v", fk)
	}
	if fk.OnDelete != schema.Cascade {
		t.Errorf("ON DELETE came back as %q, want CASCADE", fk.OnDelete)
	}

	// The enum's CHECK is imported as a CHECK. The enum-ness is NOT invented:
	// there is no enum type here, and turning `status IN ('new','paid')` back
	// into a declared label set would be pattern-matching a constraint somebody
	// may have written by hand for another reason.
	var sawCheck bool
	for _, ck := range orgs.Checks {
		if strings.Contains(strings.ToLower(ck.Expr), "status") {
			sawCheck = true
		}
	}
	if !sawCheck {
		t.Errorf("the enum's constraint did not come back: %+v", orgs.Checks)
	}
	if len(got.Enums) != 0 {
		t.Errorf("an enum was inferred from a CHECK: %+v", got.Enums)
	}
}

func tableOf(t *testing.T, s *schema.Schema, name string) *schema.Table {
	t.Helper()
	for _, tb := range s.Tables {
		if tb.Name == name {
			return tb
		}
	}
	t.Fatalf("table %s was not imported", name)
	return nil
}

func columnOf(t *testing.T, tb *schema.Table, name string) *schema.Column {
	t.Helper()
	for _, c := range tb.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s.%s was not imported", tb.Name, name)
	return nil
}
