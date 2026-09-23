package oracle

// The loaders, against a FAKE catalogue.
//
// internal/oraclespike runs a round trip against a real server, and that is
// what proves the queries are right. What it cannot do is make the catalogue
// say something awkward on purpose — a system-generated NOT NULL check, an
// index that backs a constraint, a VIRTUAL column whose expression is stored
// where a default is. Those are the rows that get imported WRONG, and this is
// where they can be handed over deliberately.
//
// The fake is possible at all because this introspector reads the VALUE side
// of the port: a row is a []any, so a canned one is a literal.

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
)

// fakeRows is the value shape of runtime.Rows over canned data.
type fakeRows struct {
	rows [][]any
	i    int
}

func (f *fakeRows) Next() bool {
	f.i++
	return f.i <= len(f.rows)
}
func (f *fakeRows) Values() []any       { return f.rows[f.i-1] }
func (f *fakeRows) RawValues() [][]byte { return nil } // the VALUE shape
func (f *fakeRows) Close()              {}
func (f *fakeRows) Err() error          { return nil }

// fakeConn answers each query with whichever canned set matches a fragment of
// its SQL, so a test names the catalogue view rather than the query order.
type fakeConn struct{ by map[string][][]any }

func (c fakeConn) Query(_ context.Context, sql string, _ []any) (runtime.Rows, error) {
	for frag, rows := range c.by {
		if strings.Contains(sql, frag) {
			return &fakeRows{rows: rows}, nil
		}
	}
	return &fakeRows{}, nil
}

func introspect(t *testing.T, by map[string][][]any) *schema.Schema {
	t.Helper()
	s, err := Introspect(context.Background(), fakeConn{by: by}, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Oracle implements NOT NULL as a CHECK with a system-generated name.
// Importing those gives every model a duplicate of what the column already
// says — and `generated = 'USER NAME'` is what excludes them, which is a
// property of the QUERY and so is asserted on the query rather than the rows.
func TestTheNotNullChecksOracleInventsAreExcludedByTheQuery(t *testing.T) {
	var seen string
	c := fakeConn{by: map[string][][]any{}}
	_ = c
	probe := probeConn{seen: &seen, match: "constraint_type = 'C'"}
	if _, err := Introspect(context.Background(), probe, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "generated = 'USER NAME'") {
		t.Errorf("the check query must exclude system-generated names, or every NOT NULL\n"+
			"column comes back as a model CHECK too:\n%s", seen)
	}
}

// An index that BACKS a constraint IS the constraint. Importing both makes
// every diff propose to drop one, forever.
func TestAnIndexBackingAConstraintIsExcludedByTheQuery(t *testing.T) {
	var seen string
	probe := probeConn{seen: &seen, match: "all_ind_columns"}
	if _, err := Introspect(context.Background(), probe, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "NOT EXISTS") || !strings.Contains(seen, "c.index_name") {
		t.Errorf("the index query must exclude constraint-backing indexes:\n%s", seen)
	}
}

type probeConn struct {
	seen  *string
	match string
}

func (p probeConn) Query(_ context.Context, sql string, _ []any) (runtime.Rows, error) {
	if strings.Contains(sql, p.match) {
		*p.seen = sql
	}
	return &fakeRows{}, nil
}

// A VIRTUAL column's expression is stored where a default is, so reading it as
// a default gives a model that inserts the expression as a literal.
func TestAVirtualColumnIsGeneratedNotDefaulted(t *testing.T) {
	s := introspect(t, map[string][][]any{
		"all_tables": {{"USERS"}},
		"all_tab_cols": {
			{"USERS", "ID", "NUMBER", int64(22), nil, int64(19), int64(0), "N", nil, "NO", "NO"},
			{"USERS", "NAME", "VARCHAR2", int64(800), int64(200), nil, nil, "N", nil, "NO", "NO"},
			{"USERS", "UPPER_NAME", "VARCHAR2", int64(800), int64(200), nil, nil, "Y",
				`UPPER("NAME")`, "YES", "NO"},
		},
	})
	tb := s.Table("users")
	if tb == nil {
		t.Fatal("no users table")
	}
	c := tb.Column("upper_name")
	if c == nil {
		t.Fatal("no upper_name column")
	}
	if c.Generated == "" {
		t.Error("a VIRTUAL column must come back generated")
	}
	if c.Default != "" {
		t.Errorf("its expression must not be read as a DEFAULT: %q", c.Default)
	}
	// And the width is CHARACTERS: data_length says 800 for a 200-character
	// column in a multi-byte character set.
	if n := tb.Column("name"); n == nil || n.Type.Size != 200 {
		t.Errorf("width came back as %+v; data_length is bytes", n)
	}
	// Names come back lowercased, because they were declared unquoted.
	if tb.Column("ID") != nil {
		t.Error("an unquoted name must be lowered")
	}
}

// An IDENTITY column's default is the sequence Oracle made for it, which is not
// a default the model declared.
func TestAnIdentityColumnCarriesNoDefault(t *testing.T) {
	s := introspect(t, map[string][][]any{
		"all_tables": {{"T"}},
		"all_tab_cols": {
			{"T", "ID", "NUMBER", int64(22), nil, int64(19), int64(0), "N",
				`"STORM"."ISEQ$$_12345".nextval`, "NO", "YES"},
		},
	})
	c := s.Table("t").Column("id")
	if !c.Identity {
		t.Fatal("the identity flag was lost")
	}
	if c.Default != "" {
		t.Errorf("a sequence Oracle invented is not a model default: %q", c.Default)
	}
}

// A function-based index's key is an EXPRESSION, and its text is SQL — not a
// name — so it is NOT folded. 'PAID' is not 'paid'.
func TestAFunctionBasedIndexKeepsItsExpressionVerbatim(t *testing.T) {
	expr := `CASE WHEN "deleted_at" IS NULL THEN "EMAIL" END`
	s := introspect(t, map[string][][]any{
		"all_tables":      {{"T"}},
		"all_tab_cols":    {{"T", "EMAIL", "VARCHAR2", int64(255), int64(255), nil, nil, "N", nil, "NO", "NO"}},
		"all_ind_columns": {{"T", "IX_LIVE", "UNIQUE", "SYS_NC00003$", "ASC", expr}},
	})
	ix := s.Table("t").Indexes
	if len(ix) != 1 {
		t.Fatalf("indexes came back as %+v", ix)
	}
	k := ix[0].Columns[0]
	if !k.Expr {
		t.Error("a function-based key must be marked as an expression")
	}
	if k.Name != expr {
		t.Errorf("the expression was altered:\n got  %s\n want %s", k.Name, expr)
	}
	if !ix[0].Unique {
		t.Error("the uniqueness was lost")
	}
}

// A CHECK's text is SQL too, and lowering it would change what it means.
func TestACheckExpressionIsNotFolded(t *testing.T) {
	s := introspect(t, map[string][][]any{
		"all_tables":            {{"T"}},
		"all_tab_cols":          {{"T", "STATUS", "VARCHAR2", int64(4), int64(4), nil, nil, "N", nil, "NO", "NO"}},
		"constraint_type = 'C'": {{"T", "CK_T_STATUS", `"STATUS" IN ('PAID','NEW')`}},
	})
	ck := s.Table("t").Checks
	if len(ck) != 1 {
		t.Fatalf("checks came back as %+v", ck)
	}
	if !strings.Contains(ck[0].Expr, "'PAID'") {
		t.Errorf("the literal was folded, which changes what it means: %s", ck[0].Expr)
	}
	// The constraint's NAME is a name, so it is folded.
	if ck[0].Name != "ck_t_status" {
		t.Errorf("the constraint name came back as %q", ck[0].Name)
	}
}

// The primary key and the unique CONSTRAINT come from one query and must not
// be confused: a P is the key, a U is a unique.
func TestKeysAndUniquesAreSeparated(t *testing.T) {
	s := introspect(t, map[string][][]any{
		"all_tables":   {{"T"}},
		"all_tab_cols": {{"T", "ID", "NUMBER", int64(22), nil, int64(19), int64(0), "N", nil, "NO", "NO"}},
		"constraint_type IN ('P','U')": {
			{"T", "PK_T", "P", "ID"},
			{"T", "UQ_T_A_B", "U", "A"},
			{"T", "UQ_T_A_B", "U", "B"},
		},
	})
	tb := s.Table("t")
	if len(tb.PrimaryKey) != 1 || tb.PrimaryKey[0] != "id" {
		t.Errorf("primary key came back as %v", tb.PrimaryKey)
	}
	if len(tb.Uniques) != 1 {
		t.Fatalf("uniques came back as %+v", tb.Uniques)
	}
	// A composite unique's columns arrive on separate rows and must be
	// gathered into one constraint, in position order.
	if got := tb.Uniques[0].Columns; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("composite unique came back as %v", got)
	}
}

// A foreign key's columns arrive on separate rows too, and NOTHING reads an ON
// UPDATE: Oracle has no such clause, so there is nothing in the catalogue.
func TestForeignKeysGatherAndCarryNoOnUpdate(t *testing.T) {
	s := introspect(t, map[string][][]any{
		"all_tables":   {{"CHILD"}, {"PARENT"}},
		"all_tab_cols": {{"CHILD", "A", "NUMBER", int64(22), nil, int64(19), int64(0), "N", nil, "NO", "NO"}},
		"constraint_type = 'R'": {
			{"CHILD", "FK_C", "A", "PARENT", "X", "CASCADE"},
			{"CHILD", "FK_C", "B", "PARENT", "Y", "CASCADE"},
		},
	})
	fks := s.Table("child").ForeignKeys
	if len(fks) != 1 {
		t.Fatalf("foreign keys came back as %+v", fks)
	}
	fk := fks[0]
	if len(fk.Columns) != 2 || fk.Columns[0] != "a" || fk.Columns[1] != "b" {
		t.Errorf("columns came back as %v", fk.Columns)
	}
	if fk.RefTable != "parent" || len(fk.RefColumns) != 2 {
		t.Errorf("reference came back as %s%v", fk.RefTable, fk.RefColumns)
	}
	if fk.OnDelete != schema.Cascade {
		t.Errorf("delete rule came back as %q", fk.OnDelete)
	}
	if fk.OnUpdate != schema.NoAction {
		t.Errorf("an ON UPDATE appeared from a dialect that has no such clause: %q", fk.OnUpdate)
	}
}

// TWO SPELLINGS FOR ONE FACT. A column that never had a default reports
// data_default as SQL NULL; one whose default was DROPPED — `MODIFY (c DEFAULT
// NULL)`, which is how Oracle drops one — reports the four characters N-U-L-L.
// Reading the second as a default value makes `storm diff` propose to drop a
// default that is already gone, forever.
func TestADroppedDefaultIsNoDefault(t *testing.T) {
	for _, stored := range []any{nil, "", "NULL", "null", "(NULL)", " NULL "} {
		s := introspect(t, map[string][][]any{
			"all_tables": {{"T"}},
			"all_tab_cols": {
				{"T", "C", "NUMBER", int64(22), nil, int64(19), int64(0), "Y", stored, "NO", "NO"},
			},
		})
		if got := s.Table("t").Column("c").Default; got != "" {
			t.Errorf("data_default %#v came back as the default %q", stored, got)
		}
	}
	// And a real default still survives.
	s := introspect(t, map[string][][]any{
		"all_tables": {{"T"}},
		"all_tab_cols": {
			{"T", "C", "NUMBER", int64(22), nil, int64(19), int64(0), "Y", "(7)", "NO", "NO"},
		},
	})
	if got := s.Table("t").Column("c").Default; got != "7" {
		t.Errorf("a real default came back as %q", got)
	}
}
