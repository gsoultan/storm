package migrate

// The Oracle seam, which is closer to SQL Server's than to PostgreSQL's and
// differs from BOTH in four places. Each is asserted here because each is a
// statement some other server would accept and this one will not.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func oraPlan(t *testing.T, from, to *schema.Schema) Plan {
	t.Helper()
	p, err := DiffFor(from, to, Oracle)
	if err != nil {
		t.Fatalf("DiffFor: %v", err)
	}
	return p
}

// (2) MODIFY restates only what CHANGED. SQL Server's ALTER COLUMN restates
// the type on every change including one that is only about nullability;
// repeating a NOT NULL a column already has is ORA-01442 here.
func TestOracleModifiesOnlyWhatChanged(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", true)))
	got := oraPlan(t, a, b).SQL()
	if !strings.Contains(got, `MODIFY ("age" NOT NULL)`) {
		t.Errorf("a nullability change must not restate the type:\n%s", got)
	}
	if strings.Contains(got, "NUMBER") {
		t.Errorf("the type was restated, which ORA-01442 refuses in some orders:\n%s", got)
	}
	if strings.Contains(got, "ALTER COLUMN") {
		t.Errorf("Oracle's is MODIFY, not ALTER COLUMN:\n%s", got)
	}

	// And the reverse direction spells NULL rather than leaving it implicit.
	back := oraPlan(t, b, a).SQL()
	if !strings.Contains(back, `MODIFY ("age" NULL)`) {
		t.Errorf("want an explicit NULL:\n%s", back)
	}
}

// A type change restates the TYPE and not the nullability, which is the other
// half of the same rule.
func TestOracleRetypeCarriesOnlyTheType(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("n", "int4", true)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("n", "int8", true)))
	got := oraPlan(t, a, b).SQL()
	if !strings.Contains(got, `MODIFY ("n" NUMBER(19))`) {
		t.Errorf("want the type alone:\n%s", got)
	}
	if strings.Contains(got, "NOT NULL") {
		t.Errorf("the nullability did not change and must not be restated:\n%s", got)
	}
}

// (3) A DEFAULT is a property of the COLUMN here, as on PostgreSQL — not a
// named constraint as on SQL Server, whose drop needs a catalogue lookup.
func TestOracleDefaultsAreColumnProperties(t *testing.T) {
	old := mcol("state", "int4", true)
	old.Default = "0"
	cur := mcol("state", "int4", true)
	cur.Default = "1"
	a := msch(mtbl("users", mcol("id", "uuid", true), old))
	b := msch(mtbl("users", mcol("id", "uuid", true), cur))

	got := oraPlan(t, a, b).SQL()
	if !strings.Contains(got, `MODIFY ("state" DEFAULT 1)`) {
		t.Errorf("want a column-property default:\n%s", got)
	}
	for _, wrong := range []string{"ADD CONSTRAINT", "sys.default_constraints", "DECLARE"} {
		if strings.Contains(got, wrong) {
			t.Errorf("%s is SQL Server's shape:\n%s", wrong, got)
		}
	}
	// And dropping one is MODIFY ... DEFAULT NULL, not a lookup.
	dropped := msch(mtbl("users", mcol("id", "uuid", true), mcol("state", "int4", true)))
	if got := oraPlan(t, a, dropped).SQL(); !strings.Contains(got, `DEFAULT NULL`) {
		t.Errorf("want DEFAULT NULL:\n%s", got)
	}
}

// (1) ADD, not ADD COLUMN.
func TestOracleAddColumnHasNoColumnKeyword(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	got := oraPlan(t, a, b).SQL()
	if strings.Contains(got, "ADD COLUMN") {
		t.Errorf("Oracle has no ADD COLUMN:\n%s", got)
	}
	if !strings.Contains(got, `ALTER TABLE "users" ADD "age"`) {
		t.Errorf("got:\n%s", got)
	}
}

// An index name is unique per SCHEMA here, not per table — so DROP INDEX takes
// no ON clause, which is the opposite of SQL Server's requirement.
func TestOracleDropIndexNamesNoTable(t *testing.T) {
	ix := &schema.Index{Name: "ix_users_age", Columns: []schema.IndexColumn{{Name: "age"}}}
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	a.Tables[0].Indexes = []*schema.Index{ix}
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	got := oraPlan(t, a, b).SQL()
	if !strings.Contains(got, `DROP INDEX "ix_users_age"`) {
		t.Errorf("got:\n%s", got)
	}
	if strings.Contains(got, " ON ") {
		t.Errorf("an index name is unique per schema here; ON is SQL Server's:\n%s", got)
	}
}

// (4) No terminating semicolon: through the protocol a trailing `;` is
// ORA-00911, which is the reason oraddl.Statements is the primitive.
func TestOracleStatementsCarryNoTerminator(t *testing.T) {
	to := msch(mtbl("users", mcol("id", "uuid", true), mcol("email", "text", true)))
	to.Tables[0].Indexes = []*schema.Index{
		{Name: "ix", Columns: []schema.IndexColumn{{Name: "email"}}},
	}
	for _, ch := range oraPlan(t, msch(), to).Changes {
		if strings.Contains(ch.SQL, ";") {
			t.Errorf("a trailing semicolon is ORA-00911 through the protocol:\n%s", ch.SQL)
		}
	}
}

// Dropping a table takes CASCADE CONSTRAINTS — a foreign key pointing AT it is
// not dropped with it and ORA-02449 refuses — and PURGE, or the table goes to
// the recycle bin under a system name whose indexes collide with a re-created
// one.
func TestOracleDropTableCascadesAndPurges(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true)))
	got := oraPlan(t, a, msch()).SQL()
	for _, want := range []string{"CASCADE CONSTRAINTS", "PURGE"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s:\n%s", want, got)
		}
	}
}

func TestOracleHasNoConcurrentIndexBuild(t *testing.T) {
	live := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	to := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	to.Tables[0].Indexes = []*schema.Index{{Name: "ix", Columns: []schema.IndexColumn{{Name: "age"}}}}
	p := oraPlan(t, live, to)
	if got := p.Concurrently(live); got.SQL() != p.SQL() {
		t.Errorf("ONLINE is an Enterprise Edition feature, as SQL Server's is:\n%s", got.SQL())
	}
}

func TestOracleUnnormalisedEnumsAreRefused(t *testing.T) {
	s := msch(mtbl("users", mcol("id", "uuid", true)))
	s.Enums = []*schema.Enum{{Name: "status", Labels: []string{"on", "off"}}}
	_, err := DiffFor(msch(), s, Oracle)
	if err == nil || !strings.Contains(err.Error(), "NormalizeOracle") {
		t.Errorf("an un-normalised model must be refused with the fix named: %v", err)
	}
}

// A new table arrives with its indexes here too: oraddl.Statements emits them
// in a second pass a diff building one table at a time never reaches.
func TestOracleNewTableCarriesItsIndexes(t *testing.T) {
	to := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	to.Tables[0].Indexes = []*schema.Index{
		{Name: "ix_users_age", Columns: []schema.IndexColumn{{Name: "age"}}},
	}
	if got := oraPlan(t, msch(), to).SQL(); !strings.Contains(got, "ix_users_age") {
		t.Errorf("a new table lost its index:\n%s", got)
	}
}
