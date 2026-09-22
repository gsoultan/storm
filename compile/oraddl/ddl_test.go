package oraddl_test

// The refusals, which are the product. A generator that emits DDL Oracle
// rejects is a generator that wastes a deploy; one that emits DDL Oracle
// ACCEPTS and that means something else is worse, and most of this file is
// about the second kind.
//
// What this file cannot prove is that the accepted DDL applies. That is
// internal/oraclespike's job and it needs a server — see P6.7.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/schema"
)

func col(name, typ string, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: typ}, NotNull: notNull}
}

func tbl(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols, PrimaryKey: []string{"id"}}
}

func sch(ts ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: ts} }

func id() *schema.Column { return col("id", schema.TypeUUID, true) }

// THE rule. Everything else in the Oracle back end rests on this being
// refused: internal/oraclespike measured that a nullable text column is the one
// place "" and NULL become the same stored value, and refusing it is what makes
// Eq("") — which can never be a declare-time error — correctly match nothing.
func TestANullableTextColumnIsRefused(t *testing.T) {
	s := sch(tbl("orgs", id(), col("note", schema.TypeText, false)))
	err := oraddl.Check(s)
	if err == nil {
		t.Fatal("a nullable text column was accepted; the empty-string rule does not fire")
	}
	for _, want := range []string{"orgs.note", "empty string", "NOT NULL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q:\n%v", want, err)
		}
	}
}

func TestANotNullTextColumnIsAccepted(t *testing.T) {
	s := sch(tbl("orgs", id(), col("name", schema.TypeText, true)))
	if err := oraddl.Check(s); err != nil {
		t.Errorf("a NOT NULL text column must port:\n%v", err)
	}
}

// A nullable VARCHAR is the same collapse as a nullable TEXT; the rule is about
// the STORED value and not about which of the two spellings declared it.
func TestTheRuleCoversVarcharAndEnumsToo(t *testing.T) {
	for _, c := range []*schema.Column{
		{Name: "x", Type: schema.Type{Name: schema.TypeVarchar, Size: 40}},
		{Name: "x", Type: schema.Type{Name: "status", Enum: true}},
	} {
		s := sch(tbl("orgs", id(), c))
		s.Enums = []*schema.Enum{{Name: "status", Labels: []string{"new", "paid"}}}
		if err := oraddl.Check(s); err == nil || !strings.Contains(err.Error(), "empty string") {
			t.Errorf("a nullable %v was accepted: %v", c.Type, err)
		}
	}
}

// A generated column is computed, never written, so nothing can put "" in it.
func TestAGeneratedTextColumnIsNotRefused(t *testing.T) {
	c := col("upper_name", schema.TypeText, false)
	c.Generated = `UPPER("name")`
	s := sch(tbl("orgs", id(), col("name", schema.TypeText, true), c))
	if err := oraddl.Check(s); err != nil {
		t.Errorf("a generated column cannot receive an empty string:\n%v", err)
	}
}

func TestDefaultEmptyStringIsRefused(t *testing.T) {
	c := col("name", schema.TypeText, true)
	c.Default = "''"
	s := sch(tbl("orgs", id(), c))
	if err := oraddl.Check(s); err == nil || !strings.Contains(err.Error(), "DEFAULT NULL") {
		t.Errorf("DEFAULT '' must be refused as DEFAULT NULL: %v", err)
	}
}

// Oracle has NO ON UPDATE clause on a foreign key. Emitting the constraint
// without it would be weaker than the model says, which is the whole failure
// mode Check exists for.
func TestOnUpdateIsRefusedByName(t *testing.T) {
	child := tbl("members", id(), col("org_id", schema.TypeUUID, true))
	child.ForeignKeys = []*schema.ForeignKey{{
		Name: "fk_members_org_id", Columns: []string{"org_id"},
		RefTable: "orgs", RefColumns: []string{"id"},
		OnDelete: schema.Cascade, OnUpdate: schema.Cascade,
	}}
	s := sch(tbl("orgs", id()), child)
	err := oraddl.Check(s)
	if err == nil || !strings.Contains(err.Error(), "ON UPDATE") {
		t.Fatalf("ON UPDATE CASCADE must be refused: %v", err)
	}
	if !strings.Contains(err.Error(), "no ON UPDATE clause") {
		t.Errorf("the refusal must say the clause does not exist, not that it is spelled "+
			"differently: %v", err)
	}
}

func TestOnDeleteCascadeAndSetNullAreEmitted(t *testing.T) {
	for action, want := range map[schema.Action]string{
		schema.Cascade:  "ON DELETE CASCADE",
		schema.SetNull:  "ON DELETE SET NULL",
		schema.Restrict: "", // no clause: refusing the delete is the default
		schema.NoAction: "",
	} {
		fk := &schema.ForeignKey{
			Name: "fk", Columns: []string{"org_id"},
			RefTable: "orgs", RefColumns: []string{"id"}, OnDelete: action,
		}
		got := oraddl.AddForeignKey(tbl("members", id()), fk)
		if want == "" {
			if strings.Contains(got, "ON DELETE") {
				t.Errorf("%s must emit no ON DELETE clause: %s", action, got)
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: want %q in %s", action, want, got)
		}
	}
}

// The soft-delete shape. A partial UNIQUE becomes a function-based index on a
// CASE, because a row whose key is entirely NULL is not indexed — which is the
// same set of rows a partial index would have constrained.
func TestAPartialUniqueBecomesACaseIndex(t *testing.T) {
	tb := tbl("users", id(), col("email", schema.TypeText, true))
	ix := &schema.Index{
		Name: "uq_users_live_email", Unique: true,
		Columns: []schema.IndexColumn{{Name: "email"}},
		Where:   `"deleted_at" IS NULL`,
	}
	got := oraddl.CreateIndex(tb, ix)
	for _, want := range []string{
		"CREATE UNIQUE INDEX",
		`CASE WHEN "deleted_at" IS NULL THEN "email" END`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "WHERE") {
		t.Errorf("Oracle has no WHERE on an index:\n%s", got)
	}
}

func TestAPartialNonUniqueIndexIsRefused(t *testing.T) {
	tb := tbl("users", id(), col("email", schema.TypeText, true))
	tb.Indexes = []*schema.Index{{
		Name: "ix_users_live", Columns: []schema.IndexColumn{{Name: "email"}},
		Where: `"deleted_at" IS NULL`,
	}}
	err := oraddl.Check(sch(tb))
	if err == nil || !strings.Contains(err.Error(), "partial and not unique") {
		t.Errorf("a partial lookup index must be refused, not silently widened: %v", err)
	}
}

func TestIdentifiersOverTheLimitAreRefused(t *testing.T) {
	long := strings.Repeat("x", 129)
	if err := oraddl.Check(sch(tbl(long, id()))); err == nil ||
		!strings.Contains(err.Error(), "128") {
		t.Errorf("a 129-character table name must be refused: %v", err)
	}
}

// Quoting preserves case, which is the point: an unquoted name folds UP here
// where PostgreSQL folds it down, so a storm model's lowercase names only stay
// lowercase because every identifier is quoted.
func TestIdentQuotesAndPreservesCase(t *testing.T) {
	if got := oraddl.Ident("users"); got != `"users"` {
		t.Errorf(`Ident("users") = %s, want "users" quoted`, got)
	}
	if got := oraddl.Ident(`a"b`); got != `"a""b"` {
		t.Errorf("a quote in a name must be doubled, got %s", got)
	}
}

// Every type that crosses, and the ones that do not. The table is the
// documentation: a reader should be able to see the whole mapping here.
func TestTypeMapping(t *testing.T) {
	ok := map[string]string{
		schema.TypeBool:        "BOOLEAN",
		schema.TypeInt2:        "NUMBER(5)",
		schema.TypeInt4:        "NUMBER(10)",
		schema.TypeInt8:        "NUMBER(19)",
		schema.TypeFloat4:      "BINARY_FLOAT",
		schema.TypeFloat8:      "BINARY_DOUBLE",
		schema.TypeNumeric:     "NUMBER",
		schema.TypeText:        "CLOB",
		schema.TypeBytea:       "BLOB",
		schema.TypeUUID:        "RAW(16)",
		schema.TypeTimestamptz: "TIMESTAMP(6) WITH TIME ZONE",
		schema.TypeTimestamp:   "TIMESTAMP(6)",
		schema.TypeDate:        "DATE",
		schema.TypeJSONB:       "JSON",
	}
	for name, want := range ok {
		got, err := oraddl.TypeSQL("t", col("c", name, true))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s → %s, want %s", name, got, want)
		}
	}

	for _, name := range []string{
		schema.TypeTime, schema.TypeInterval, schema.TypeInet, schema.TypeCIDR,
		schema.TypeMacaddr, schema.TypeTSVector, schema.TypeTstzRange, schema.TypeHstore,
	} {
		if _, err := oraddl.TypeSQL("t", col("c", name, true)); err == nil {
			t.Errorf("%s was accepted and has no Oracle form", name)
		}
	}
}

// Unlike MySQL and SQL Server, an unspecified numeric is FINE here — and the
// reason is worth pinning, because the refusal those two make looks like a
// house style rather than what it is. Their unspecified DECIMAL means scale
// zero and truncates every fraction; Oracle's bare NUMBER keeps it.
func TestUnboundedNumericIsAcceptedHereAndNotElsewhere(t *testing.T) {
	got, err := oraddl.TypeSQL("t", col("amount", schema.TypeNumeric, true))
	if err != nil {
		t.Fatalf("a bare numeric must port to Oracle: %v", err)
	}
	if got != "NUMBER" {
		t.Errorf("want NUMBER, got %s", got)
	}
}

func TestVarcharNeedsASizeAndHasACeiling(t *testing.T) {
	unsized := &schema.Column{Name: "c", Type: schema.Type{Name: schema.TypeVarchar}}
	if _, err := oraddl.TypeSQL("t", unsized); err == nil {
		t.Error("an unbounded VARCHAR2 does not exist without MAX_STRING_SIZE=EXTENDED")
	}
	big := &schema.Column{Name: "c", Type: schema.Type{Name: schema.TypeVarchar, Size: 8000}}
	if _, err := oraddl.TypeSQL("t", big); err == nil {
		t.Error("VARCHAR2 stops at 4000 and 8000 was accepted")
	}
	sized := &schema.Column{Name: "c", Type: schema.Type{Name: schema.TypeVarchar, Size: 200}}
	got, err := oraddl.TypeSQL("t", sized)
	if err != nil {
		t.Fatal(err)
	}
	// CHAR semantics spelled out: a bare VARCHAR2(n) counts BYTES, so a name
	// with an accent in it fits fewer characters than the model said.
	if got != "VARCHAR2(200 CHAR)" {
		t.Errorf("want character semantics, got %s", got)
	}
}

// Both uuid defaults are ACCEPTED and neither is emitted: the key is generated
// client-side, as it is on MySQL. SYS_GUID() looks available and is not a uuid
// — Oracle documents it as host-and-sequence derived — so a column defaulted to
// it would be guessable where the model asked for random and unsorted where it
// asked for time-ordered.
func TestUUIDDefaultsAreClientSideAndEmitNothing(t *testing.T) {
	for _, def := range []string{"uuidv7()", "gen_random_uuid()"} {
		c := id()
		c.Default = def
		if err := oraddl.Check(sch(tbl("orgs", c))); err != nil {
			t.Errorf("%s must port — storm generates it client-side: %v", def, err)
		}
		got, err := oraddl.ColumnDef("orgs", c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "DEFAULT") || strings.Contains(got, "SYS_GUID") {
			t.Errorf("%s must emit no default: %s", def, got)
		}
	}
}

// Check reports EVERY problem, not the first. Finding them one deploy at a
// time is the failure mode it exists to replace.
func TestCheckReportsEveryProblemAtOnce(t *testing.T) {
	s := sch(tbl("orgs", id(),
		col("a", schema.TypeText, false),
		col("b", schema.TypeText, false),
		col("c", schema.TypeInet, true)))
	err := oraddl.Check(s)
	if err == nil {
		t.Fatal("three problems were accepted")
	}
	for _, want := range []string{"orgs.a", "orgs.b", "orgs.c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s is missing from a one-pass report:\n%v", want, err)
		}
	}
}

func TestCreateRendersAWholeSchema(t *testing.T) {
	orgs := tbl("orgs", id(), col("name", schema.TypeVarchar, true))
	orgs.Columns[1].Type.Size = 200
	orgs.Indexes = []*schema.Index{{
		Name: "ix_orgs_name", Columns: []schema.IndexColumn{{Name: "name"}},
	}}
	members := tbl("members", id(), col("org_id", schema.TypeUUID, true))
	members.ForeignKeys = []*schema.ForeignKey{{
		Name: "fk_members_org_id", Columns: []string{"org_id"},
		RefTable: "orgs", RefColumns: []string{"id"}, OnDelete: schema.Cascade,
	}}

	stmts, err := oraddl.Statements(sch(orgs, members))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(stmts, "\n")
	for _, want := range []string{
		`CREATE TABLE "orgs"`,
		`"name" VARCHAR2(200 CHAR) NOT NULL`,
		`PRIMARY KEY ("id")`,
		`CREATE INDEX "ix_orgs_name" ON "orgs" ("name")`,
		`ALTER TABLE "members" ADD CONSTRAINT "fk_members_org_id"`,
		"ON DELETE CASCADE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// No semicolons. Through TTC a trailing `;` is ORA-00911, unlike every
	// other target storm has where it is required or harmless.
	for _, st := range stmts {
		if strings.Contains(st, ";") {
			t.Errorf("a trailing semicolon is ORA-00911 through the protocol:\n%s", st)
		}
	}

	// And the FILE form, which is the other medium and the other rule: a
	// migration runner splits on exactly the terminator the protocol refuses.
	file, err := oraddl.Create(sch(orgs, members))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(file, ";"); n != len(stmts) {
		t.Errorf("a migration file needs one terminator per statement: %d for %d", n, len(stmts))
	}
}
