package migrate

// The scratch-name rewrite, which is where the first live run failed.
//
// Oracle's scratch namespace is a name PREFIX in the connected user's own
// schema — a schema IS a user here, so a real one would mean CREATE USER and a
// privilege an application's account will not have. That makes these two pure
// functions load-bearing in a way the other two targets' normalisers have no
// equivalent of, and ORA-02264 on the first run was one of them being wrong.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
)

func oraFixture() *schema.Schema {
	orgs := &schema.Table{
		Name:       "orgs",
		PrimaryKey: []string{"id"},
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "name", Type: schema.Type{Name: schema.TypeVarchar, Size: 200}, NotNull: true},
		},
		Uniques: []*schema.Unique{{Name: "uq_orgs_name", Columns: []string{"name"}}},
		Checks:  []*schema.Check{{Name: "ck_orgs_name", Expr: `LENGTH("name") > 0`}},
		Indexes: []*schema.Index{{Name: "ix_orgs_name",
			Columns: []schema.IndexColumn{{Name: "name"}}}},
	}
	members := &schema.Table{
		Name:       "members",
		PrimaryKey: []string{"id"},
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "org_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "fk_members_org_id", Columns: []string{"org_id"},
			RefTable: "orgs", RefColumns: []string{"id"},
		}},
	}
	return &schema.Schema{Tables: []*schema.Table{orgs, members}}
}

// EVERY name, not just the table's. A constraint and an index are
// schema-scoped here, not table-scoped — so a scratch table called
// sn_1_orgs still carrying a unique called uq_orgs_name collides with the live
// table's, which is ORA-02264. PostgreSQL and SQL Server both scope these to
// the table, so this is the first target where prefixing a table is not enough.
func TestPrefixRewritesEverySchemaScopedName(t *testing.T) {
	got, names, err := prefixed(oraFixture(), "sn_1_")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "sn_1_orgs" {
		t.Errorf("dropped names came back as %v", names)
	}
	orgs := got.Tables[0]
	for what, name := range map[string]string{
		"table":       orgs.Name,
		"unique":      orgs.Uniques[0].Name,
		"check":       orgs.Checks[0].Name,
		"index":       orgs.Indexes[0].Name,
		"foreign key": got.Tables[1].ForeignKeys[0].Name,
		"fk target":   got.Tables[1].ForeignKeys[0].RefTable,
	} {
		if !strings.HasPrefix(name, "sn_1_") {
			t.Errorf("the %s name %q was not prefixed; a schema-scoped name that is not "+
				"is ORA-02264 against the live object", what, name)
		}
	}
}

// The original must not be touched: it is the caller's model, and normalising
// is not supposed to be observable.
func TestPrefixDoesNotMutateTheCallersModel(t *testing.T) {
	in := oraFixture()
	if _, _, err := prefixed(in, "sn_1_"); err != nil {
		t.Fatal(err)
	}
	if in.Tables[0].Name != "orgs" {
		t.Errorf("the table was renamed in place: %s", in.Tables[0].Name)
	}
	if in.Tables[0].Uniques[0].Name != "uq_orgs_name" {
		t.Errorf("the unique was renamed in place: %s", in.Tables[0].Uniques[0].Name)
	}
	if in.Tables[1].ForeignKeys[0].RefTable != "orgs" {
		t.Errorf("the foreign key's target was rewritten in place: %s",
			in.Tables[1].ForeignKeys[0].RefTable)
	}
}

// Oracle's identifier limit is 128 and a prefixed table can exceed it. A
// refusal names the table the CALLER wrote, not the scratch one.
func TestATableTooLongToPrefixIsRefused(t *testing.T) {
	long := strings.Repeat("x", 126)
	s := &schema.Schema{Tables: []*schema.Table{{Name: long}}}
	_, _, err := prefixed(s, "sn_1_")
	if err == nil {
		t.Fatal("a name past 128 characters must be refused")
	}
	if !strings.Contains(err.Error(), "128") || strings.Contains(err.Error(), "sn_1_"+long) {
		t.Errorf("the refusal must name the limit and the CALLER's table: %v", err)
	}
}

// The inverse removes the prefix from ANYWHERE in a name, because the two ways
// a name acquires it put it in different places: a declared uq_orgs_name
// becomes sn_1_uq_orgs_name, and an enum's check — derived by oraddl from the
// already-prefixed table — becomes ck_sn_1_orgs_status.
func TestUnprefixHandlesBothPlacements(t *testing.T) {
	in := &schema.Schema{Tables: []*schema.Table{{
		Name:       "sn_1_orgs",
		PrimaryKey: []string{"id"},
		Columns:    []*schema.Column{{Name: "id", Type: schema.Type{Name: schema.TypeUUID}}},
		Uniques:    []*schema.Unique{{Name: "sn_1_uq_orgs_name", Columns: []string{"name"}}},
		Checks:     []*schema.Check{{Name: "ck_sn_1_orgs_status", Expr: "1=1"}},
		Indexes: []*schema.Index{{Name: "ck_sn_1_ix", Columns: []schema.IndexColumn{
			{Name: `CASE WHEN "d" IS NULL THEN "sn_1_orgs"."e" END`, Expr: true},
		}}},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "sn_1_fk_x", RefTable: "sn_1_orgs", Columns: []string{"a"},
		}},
	}}}
	got := unprefixed(in, "sn_1_")
	tb := got.Table("orgs")
	if tb == nil {
		t.Fatal("the table did not come back")
	}
	for what, name := range map[string]string{
		"leading prefix":  tb.Uniques[0].Name,
		"embedded prefix": tb.Checks[0].Name,
		"foreign key":     tb.ForeignKeys[0].Name,
		"fk target":       tb.ForeignKeys[0].RefTable,
	} {
		if strings.Contains(name, "sn_1_") {
			t.Errorf("the %s kept its prefix: %q", what, name)
		}
	}
	if strings.Contains(tb.Indexes[0].Columns[0].Name, "sn_1_") {
		t.Errorf("an index EXPRESSION kept the prefix: %q", tb.Indexes[0].Columns[0].Name)
	}
}

// Tables the prefix does not match are DROPPED: the connected user's own
// schema holds the application's real tables, and normalisation must return
// the MODEL's shape and nothing else.
func TestUnprefixDropsTheApplicationsOwnTables(t *testing.T) {
	in := &schema.Schema{Tables: []*schema.Table{
		{Name: "sn_1_orgs", PrimaryKey: []string{"id"}},
		{Name: "orgs", PrimaryKey: []string{"id"}},      // the live one
		{Name: "audit_log", PrimaryKey: []string{"id"}}, // somebody else's
	}}
	got := unprefixed(in, "sn_1_")
	if len(got.Tables) != 1 || got.Tables[0].Name != "orgs" {
		var names []string
		for _, t := range got.Tables {
			names = append(names, t.Name)
		}
		t.Errorf("normalisation returned %v; it must return the model's shape and "+
			"nothing else", names)
	}
}

// ---- NormalizeOracle's refusals, which need no server ------------------------

type oraFakeConn struct {
	execErr  error
	queryErr error
	execs    []string
}

func (c *oraFakeConn) Exec(_ context.Context, sql string, _ []any) (int64, error) {
	c.execs = append(c.execs, sql)
	return 0, c.execErr
}

func (c *oraFakeConn) Query(_ context.Context, _ string, _ []any) (runtime.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return emptyOraRows{}, nil
}

type emptyOraRows struct{}

func (emptyOraRows) Next() bool          { return false }
func (emptyOraRows) Values() []any       { return nil }
func (emptyOraRows) RawValues() [][]byte { return nil }
func (emptyOraRows) Close()              {}
func (emptyOraRows) Err() error          { return nil }

// A model Oracle cannot express is refused BEFORE anything is applied, and the
// message names the caller's table rather than the scratch one — the scratch
// mechanism leaking into the sentence somebody has to act on is the one place
// it must not.
func TestNormalizeOracleRefusesWithTheCallersNames(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{
		Name:       "orgs",
		PrimaryKey: []string{"id"},
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			// Nullable text: the empty-string rule.
			{Name: "note", Type: schema.Type{Name: schema.TypeText}},
		},
	}}}
	c := &oraFakeConn{}
	_, err := NormalizeOracle(context.Background(), c, s)
	if err == nil {
		t.Fatal("a nullable text column must be refused")
	}
	if !strings.Contains(err.Error(), "orgs.note") {
		t.Errorf("the refusal must name the caller's column: %v", err)
	}
	if strings.Contains(err.Error(), oraScratchPrefix) {
		t.Errorf("the scratch prefix leaked into the message: %v", err)
	}
	// Nothing was applied, but the drop DID run — the leftover sweep is
	// unconditional, because a crashed earlier run is exactly what it is for.
	for _, e := range c.execs {
		if strings.HasPrefix(e, "CREATE") {
			t.Errorf("DDL was applied for a model that does not port: %s", e)
		}
	}
}

// A server that refuses the DDL is reported WITH the statement, because
// "apply model DDL" on its own names nothing a reader can look at.
func TestNormalizeOracleNamesTheStatementTheServerRefused(t *testing.T) {
	c := &oraFakeConn{execErr: errors.New("ORA-00955: name is already used")}
	_, err := NormalizeOracle(context.Background(), c, oraFixture())
	if err == nil {
		t.Fatal("a refused statement must reach the caller")
	}
	if !strings.Contains(err.Error(), "ORA-00955") {
		t.Errorf("the server's error must survive: %v", err)
	}
	if !strings.Contains(err.Error(), "CREATE TABLE") {
		t.Errorf("the refusal must name the statement: %v", err)
	}
}

// Everything is dropped on every exit path, including failure — and the drop
// runs BEFORE the apply too, so a crashed earlier run with this pid cleans up.
func TestNormalizeOracleDropsOnEveryPath(t *testing.T) {
	c := &oraFakeConn{queryErr: errors.New("catalogue unavailable")}
	if _, err := NormalizeOracle(context.Background(), c, oraFixture()); err == nil {
		t.Fatal("a failed introspection must reach the caller")
	}
	var drops int
	for _, e := range c.execs {
		if strings.HasPrefix(e, "DROP TABLE") {
			drops++
		}
	}
	// Two tables, dropped before the apply and again on the way out.
	if drops != 4 {
		t.Errorf("got %d drops, want 4 (two tables, swept before and after):\n%v",
			drops, c.execs)
	}
	for _, e := range c.execs {
		if strings.HasPrefix(e, "DROP TABLE") && !strings.Contains(e, "CASCADE CONSTRAINTS") {
			t.Errorf("a drop without CASCADE CONSTRAINTS is ORA-02449 when a foreign "+
				"key points at it: %s", e)
		}
	}
}
