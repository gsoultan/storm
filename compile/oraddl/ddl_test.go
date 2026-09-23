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

// Enums: there is no enum TYPE here, so the labels are a VARCHAR2 wide enough
// for the longest one plus a CHECK that makes it an enum rather than any
// string. Same cut msddl makes, and for the same reason.
func TestAnEnumIsAWidthAndACheck(t *testing.T) {
	e := &schema.Enum{Name: "status", Labels: []string{"new", "paid", "cancelled"}}
	// CHARACTERS, not bytes: a label with an accent is two bytes and one
	// character, and a byte-counted column would refuse it.
	if got := oraddl.TypeEnum(e); got != "VARCHAR2(9 CHAR)" {
		t.Errorf("TypeEnum = %s, want the widest label as characters", got)
	}
	got := oraddl.EnumCheck("status", e)
	for _, want := range []string{`"status" IN (`, "'new'", "'paid'", "'cancelled'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	// A label with a quote in it must not end the literal.
	q := &schema.Enum{Name: "s", Labels: []string{"it's"}}
	if c := oraddl.EnumCheck("s", q); !strings.Contains(c, "'it''s'") {
		t.Errorf("a quote in a label must be doubled: %s", c)
	}
}

func TestAnEnumColumnRendersAsItsCheckedWidth(t *testing.T) {
	enums := map[string]*schema.Enum{
		"status": {Name: "status", Labels: []string{"new", "paid"}},
	}
	c := &schema.Column{Name: "status", Type: schema.Type{Name: "status", Enum: true}, NotNull: true}
	got, err := oraddl.ColumnDef("orders", c, enums)
	if err != nil {
		t.Fatal(err)
	}
	if got != `"status" VARCHAR2(4 CHAR) NOT NULL` {
		t.Errorf("got %s", got)
	}
	if ty, err := oraddl.ColumnType("orders", c, enums); err != nil || ty != "VARCHAR2(4 CHAR)" {
		t.Errorf("ColumnType = %s, %v", ty, err)
	}
	// An enum nothing declared is an error rather than a guessed width.
	if _, err := oraddl.ColumnDef("orders", c, nil); err == nil {
		t.Error("an undeclared enum must be refused")
	}
}

// The table-level CHECK travels with the table, which is the whole of what an
// enum means where there is no enum type.
func TestCreateTableCarriesTheEnumCheck(t *testing.T) {
	enums := map[string]*schema.Enum{
		"status": {Name: "status", Labels: []string{"new", "paid"}},
	}
	tb := tbl("orders", id(),
		&schema.Column{Name: "status", Type: schema.Type{Name: "status", Enum: true}, NotNull: true})
	got, err := oraddl.CreateTable(tb, enums)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `CONSTRAINT "ck_orders_status" CHECK (`) {
		t.Errorf("the enum's CHECK is missing:\n%s", got)
	}
}

// A generated column is VIRTUAL — Oracle's only form — and names no type,
// because Oracle derives it.
func TestAGeneratedColumnIsVirtual(t *testing.T) {
	c := col("upper_name", schema.TypeText, true)
	c.Generated = `UPPER("name")`
	got, err := oraddl.ColumnDef("orgs", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "GENERATED ALWAYS AS") || !strings.Contains(got, "VIRTUAL") {
		t.Errorf("got %s", got)
	}
	if strings.Contains(got, "CLOB") {
		t.Errorf("a virtual column names no type; Oracle derives it: %s", got)
	}
}

// Serial and Identity collapse: Oracle has only the one form, and BY DEFAULT
// rather than ALWAYS because storm's write path may supply the key and ALWAYS
// refuses that with ORA-32795.
func TestIdentityAndSerialBothBecomeGeneratedByDefault(t *testing.T) {
	for _, c := range []*schema.Column{
		{Name: "id", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, Identity: true},
		{Name: "id", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, Serial: true},
	} {
		got, err := oraddl.ColumnDef("t", c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "GENERATED BY DEFAULT AS IDENTITY") {
			t.Errorf("got %s", got)
		}
		if strings.Contains(got, "ALWAYS AS IDENTITY") {
			t.Errorf("ALWAYS refuses a caller-supplied key with ORA-32795: %s", got)
		}
	}
}

func TestTheDefaultsStormGeneratesAreTranslated(t *testing.T) {
	for in, want := range map[string]string{
		"now()":             "SYSTIMESTAMP",
		"CURRENT_TIMESTAMP": "SYSTIMESTAMP",
		"7":                 "7",
	} {
		c := col("c", schema.TypeTimestamptz, true)
		c.Default = in
		got, err := oraddl.ColumnDef("t", c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "DEFAULT "+want) {
			t.Errorf("%s became %s", in, got)
		}
	}
}

// An arc's check is STORM's own SQL and its PostgreSQL spelling casts a
// boolean with `::int`, which parses nowhere else — so it is respelled. A
// DECLARED check is the model's and passes through unchanged.
func TestAnArcCheckIsRespelledAndADeclaredOneIsNot(t *testing.T) {
	tb := tbl("events", id(),
		col("a_id", schema.TypeUUID, false), col("b_id", schema.TypeUUID, false))
	tb.Checks = []*schema.Check{
		{Name: "ck_arc", Arc: []string{"a_id", "b_id"}},
		{Name: "ck_mine", Expr: `"a_id" IS NOT NULL`},
	}
	// The arc's columns are nullable text? No — uuid, so the empty-string rule
	// does not fire and Check passes.
	got, err := oraddl.CreateTable(tb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CASE WHEN") || strings.Contains(got, "::int") {
		t.Errorf("an arc check must be respelled without the PostgreSQL cast:\n%s", got)
	}
	if !strings.Contains(got, `CHECK ("a_id" IS NOT NULL)`) {
		t.Errorf("a declared check passes through unchanged:\n%s", got)
	}
}

// An index with no declared name gets a derived one, and it has to be the
// same one twice or a diff proposes to recreate it forever.
func TestAnUnnamedIndexGetsAStableDerivedName(t *testing.T) {
	tb := tbl("users", id(), col("email", schema.TypeText, true))
	ix := &schema.Index{Columns: []schema.IndexColumn{{Name: "email"}}}
	a := oraddl.CreateIndex(tb, ix)
	b := oraddl.CreateIndex(tb, ix)
	if a != b {
		t.Errorf("the derived name is not stable:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, `"ix_users_email"`) {
		t.Errorf("got %s", a)
	}
}

// Routines are refused as a group: PL/SQL is a different language from
// PL/pgSQL and storm will not translate one into the other.
func TestRoutinesAreRefused(t *testing.T) {
	s := sch(tbl("t", id()))
	s.Views = []*schema.View{{Name: "v", Query: "SELECT 1 FROM dual"}}
	err := oraddl.Check(s)
	if err == nil || !strings.Contains(err.Error(), "PL/SQL") {
		t.Errorf("routines must be refused with the reason: %v", err)
	}
}

// Every index fact that has no Oracle form, refused by name in ONE pass.
func TestTheIndexFactsWithNoOracleFormAreRefused(t *testing.T) {
	for name, ix := range map[string]*schema.Index{
		"method":  {Name: "i", Method: "gin", Columns: []schema.IndexColumn{{Name: "email"}}},
		"include": {Name: "i", Columns: []schema.IndexColumn{{Name: "email"}}, Include: []string{"id"}},
		"nulls not distinct": {Name: "i", NullsNotDistinct: true,
			Columns: []schema.IndexColumn{{Name: "email"}}},
		"storage params": {Name: "i", Columns: []schema.IndexColumn{{Name: "email"}},
			With: []schema.StorageParam{{Name: "fillfactor", Value: "70"}}},
		"nulls placement": {Name: "i",
			Columns: []schema.IndexColumn{{Name: "email", NullsFirst: true}}},
		"opclass": {Name: "i",
			Columns: []schema.IndexColumn{{Name: "email", OpClass: "text_pattern_ops"}}},
	} {
		tb := tbl("users", id(), col("email", schema.TypeText, true))
		tb.Indexes = []*schema.Index{ix}
		if err := oraddl.Check(sch(tb)); err == nil {
			t.Errorf("%s was accepted and has no Oracle form", name)
		}
	}
}

// A table with no primary key has nothing for a foreign key to reference and
// nothing for storm's write path to identify a row by.
func TestATableWithNoPrimaryKeyIsRefused(t *testing.T) {
	tb := &schema.Table{Name: "t", Columns: []*schema.Column{id()}}
	if err := oraddl.Check(sch(tb)); err == nil ||
		!strings.Contains(err.Error(), "primary key") {
		t.Errorf("got %v", err)
	}
}

// EXCLUDE constraints and partitioning: the first has no equivalent, the
// second is a licensed option on some editions and storm will not emit DDL
// that works on the vendor's database and is a licensing question on the
// customer's.
func TestExcludesAndPartitioningAreRefused(t *testing.T) {
	ex := tbl("t", id())
	ex.Excludes = []*schema.Exclude{{Name: "x"}}
	if err := oraddl.Check(sch(ex)); err == nil || !strings.Contains(err.Error(), "EXCLUDE") {
		t.Errorf("got %v", err)
	}
	part := tbl("t", id())
	part.Partition = &schema.Partition{Strategy: "RANGE", Columns: []string{"id"}}
	if err := oraddl.Check(sch(part)); err == nil ||
		!strings.Contains(err.Error(), "partitioning") {
		t.Errorf("got %v", err)
	}
}

func TestSetDefaultOnDeleteIsRefused(t *testing.T) {
	child := tbl("members", id(), col("org_id", schema.TypeUUID, true))
	child.ForeignKeys = []*schema.ForeignKey{{
		Name: "fk", Columns: []string{"org_id"}, RefTable: "orgs",
		RefColumns: []string{"id"}, OnDelete: schema.SetDefault,
	}}
	err := oraddl.Check(sch(tbl("orgs", id()), child))
	if err == nil || !strings.Contains(err.Error(), "SET DEFAULT") {
		t.Errorf("CASCADE and SET NULL are the only two: %v", err)
	}
}

// Storm's OWN live-rows predicate is respelled; a DECLARED one is not.
//
// The predicate storm writes for a soft-delete table's unique is a BARE column
// name — `deleted_at IS NULL` — which folds to lowercase on PostgreSQL and
// matches. It folds UP here, against a column storm itself created as
// "deleted_at", and is ORA-00904. A declared Where is the model's own SQL and
// every back end passes it through unchanged.
func TestStormsOwnPartialPredicateIsQuotedAndTheModelsIsNot(t *testing.T) {
	tb := tbl("users", id(), col("email", schema.TypeText, true))

	// Storm's, marked by LiveCol.
	mine := &schema.Index{
		Name: "uq_users_email", Unique: true,
		Columns: []schema.IndexColumn{{Name: "email"}},
		Where:   "deleted_at IS NULL", LiveCol: "deleted_at",
	}
	got := oraddl.CreateIndex(tb, mine)
	if !strings.Contains(got, `CASE WHEN "deleted_at" IS NULL`) {
		t.Errorf("storm's own predicate must be quoted, or it is ORA-00904:\n%s", got)
	}

	// The model's, passed through.
	theirs := &schema.Index{
		Name: "ix_users_live", Unique: true,
		Columns: []schema.IndexColumn{{Name: "email"}},
		Where:   `UPPER("email") <> 'X'`,
	}
	got = oraddl.CreateIndex(tb, theirs)
	if !strings.Contains(got, `CASE WHEN UPPER("email") <> 'X'`) {
		t.Errorf("a declared predicate is the model's SQL and passes through:\n%s", got)
	}
}

// PostgreSQL and SQL Server allow a duplicate index and only waste space;
// Oracle refuses it with ORA-01408. So a model carrying one applies everywhere
// else and fails here — which is exactly what a portability Check is for.
func TestADuplicateIndexIsRefused(t *testing.T) {
	tb := tbl("users", id(), col("email", schema.TypeText, true))
	tb.Indexes = []*schema.Index{
		{Name: "a", Unique: true, Columns: []schema.IndexColumn{{Name: "email"}}},
		{Name: "b", Columns: []schema.IndexColumn{{Name: "email"}}},
	}
	err := oraddl.Check(sch(tb))
	if err == nil || !strings.Contains(err.Error(), "ORA-01408") {
		t.Fatalf("a duplicate index must be refused: %v", err)
	}
	// And the message names the way it is usually produced by accident.
	if !strings.Contains(err.Error(), "soft-delete") {
		t.Errorf("the refusal should say how this happens without noticing: %v", err)
	}

	// Two indexes over the same columns with DIFFERENT predicates are two
	// different indexes and must both survive.
	tb.Indexes[1].Where = `"email" <> 'x'`
	if err := oraddl.Check(sch(tb)); err != nil {
		t.Errorf("different predicates are different indexes: %v", err)
	}
}
