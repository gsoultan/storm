package msddl_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/schema"
)

func col(name string, t schema.Type, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: t, NotNull: notNull}
}

// The types that cross, and what they become. Named rather than golden, because
// what matters about each is a decision — NVARCHAR over VARCHAR, DATETIMEOFFSET
// over DATETIME2 — and a golden file records the bytes without the reason.
func TestTypesThatCross(t *testing.T) {
	for _, c := range []struct {
		ty   schema.Type
		want string
		why  string
	}{
		{schema.Type{Name: schema.TypeBool}, "BIT", "SQL Server's boolean"},
		{schema.Type{Name: schema.TypeInt4}, "INT", ""},
		{schema.Type{Name: schema.TypeInt8}, "BIGINT", ""},
		{schema.Type{Name: schema.TypeFloat4}, "REAL", ""},
		{schema.Type{Name: schema.TypeFloat8}, "FLOAT(53)",
			"a bare FLOAT is 53 bits but FLOAT(n<=24) silently becomes REAL"},
		{schema.Type{Name: schema.TypeNumeric, Precision: 19, Scale: 4}, "DECIMAL(19,4)", ""},
		{schema.Type{Name: schema.TypeText}, "NVARCHAR(MAX)",
			"VARCHAR is bytes in the database's code page; what it stores depends on who ran setup"},
		{schema.Type{Name: schema.TypeVarchar, Size: 255}, "NVARCHAR(255)", ""},
		{schema.Type{Name: schema.TypeVarchar}, "NVARCHAR(MAX)", "no declared size"},
		{schema.Type{Name: schema.TypeBytea}, "VARBINARY(MAX)", ""},
		{schema.Type{Name: schema.TypeUUID}, "UNIQUEIDENTIFIER",
			"native, and 16 bytes — no BINARY(16) substitution the way MySQL needs"},
		{schema.Type{Name: schema.TypeTimestamptz}, "DATETIMEOFFSET(7)",
			"carries the offset, which is what distinguishes it from DATETIME2"},
		{schema.Type{Name: schema.TypeTimestamp}, "DATETIME2(7)", ""},
		{schema.Type{Name: schema.TypeDate}, "DATE", ""},
		{schema.Type{Name: schema.TypeTime}, "TIME(7)", ""},
		{schema.Type{Name: schema.TypeJSONB}, "NVARCHAR(MAX)",
			"through the 2019 level there is no json TYPE, only json functions over text"},
	} {
		got, err := msddl.TypeSQL("t", col("c", c.ty, true))
		if err != nil {
			t.Errorf("%s: %v", c.ty.Name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s -> %s, want %s (%s)", c.ty.Name, got, c.want, c.why)
		}
	}
}

// The ones that do not cross. Every message has to name a FIX, because an
// adopter who hits one is deciding what to do rather than reading a diagnosis.
func TestTypesThatDoNotCrossNameAFix(t *testing.T) {
	for _, ty := range []schema.Type{
		{Name: schema.TypeInterval},
		{Name: schema.TypeInet},
		{Name: schema.TypeCIDR},
		{Name: schema.TypeMacaddr},
		{Name: schema.TypeTSVector},
		{Name: schema.TypeTstzRange},
		{Name: schema.TypeHstore},
		{Name: schema.TypeText, Array: true},
	} {
		_, err := msddl.TypeSQL("t", col("c", ty, true))
		if err == nil {
			t.Errorf("%s was accepted; SQL Server has no such type", ty.Name)
			continue
		}
		if !strings.Contains(err.Error(), "—") {
			t.Errorf("%s: the refusal names no fix: %s", ty.Name, err)
		}
	}
}

// An unbounded DECIMAL is refused for the reason MySQL's is: unspecified means
// DECIMAL(18,0) here, so every fraction is truncated, silently, in exactly the
// column where that is unacceptable.
func TestUnboundedDecimalIsRefused(t *testing.T) {
	_, err := msddl.TypeSQL("invoices", col("amount", schema.Type{Name: schema.TypeNumeric}, true))
	if err == nil {
		t.Fatal("an unbounded DECIMAL was accepted")
	}
	if !strings.Contains(err.Error(), "DECIMAL(18,0)") {
		t.Errorf("the refusal does not say what it would have become: %s", err)
	}
}

// PostgreSQL's UNIQUE treats NULLs as DISTINCT — many NULL rows are fine. SQL
// Server's treats them as EQUAL and accepts one. That changes ANSWERS, so the
// index is TRANSLATED into the filtered form that means what PostgreSQL's
// means, rather than emitted as-is or refused.
func TestNullableUniqueBecomesAFilteredIndex(t *testing.T) {
	tb := &schema.Table{
		Name: "users",
		Columns: []*schema.Column{
			col("id", schema.Type{Name: schema.TypeUUID}, true),
			col("email", schema.Type{Name: schema.TypeVarchar, Size: 255}, false),
		},
		PrimaryKey: []string{"id"},
	}
	ix := &schema.Index{Name: "uq_users_email", Unique: true,
		Columns: []schema.IndexColumn{{Name: "email"}}}

	got := msddl.CreateIndex(tb, ix)
	if !strings.Contains(got, "WHERE [email] IS NOT NULL") {
		t.Errorf("a nullable unique was emitted unfiltered, so SQL Server would refuse a "+
			"second NULL row that PostgreSQL accepts:\n  %s", got)
	}

	// NOT NULL: the two servers already agree, so no filter is added.
	tb.Columns[1].NotNull = true
	if got := msddl.CreateIndex(tb, ix); strings.Contains(got, "WHERE") {
		t.Errorf("a non-nullable unique gained a filter it does not need:\n  %s", got)
	}

	// NULLS NOT DISTINCT is a PostgreSQL model asking for NULLs to CONFLICT,
	// which is SQL Server's plain unique. myddl has to refuse this outright.
	tb.Columns[1].NotNull = false
	ix.NullsNotDistinct = true
	if got := msddl.CreateIndex(tb, ix); strings.Contains(got, "WHERE") {
		t.Errorf("NULLS NOT DISTINCT is this server's default and needs no filter:\n  %s", got)
	}
}

// INCLUDE exists here. MySQL has no covering clause and myddl refuses one.
func TestCoveringIndexIsEmitted(t *testing.T) {
	tb := &schema.Table{Name: "orders", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true),
		col("status", schema.Type{Name: schema.TypeVarchar, Size: 20}, true),
		col("total", schema.Type{Name: schema.TypeNumeric, Precision: 12, Scale: 2}, true),
	}, PrimaryKey: []string{"id"}}
	ix := &schema.Index{Name: "ix_orders_status", Columns: []schema.IndexColumn{{Name: "status"}},
		Include: []string{"total"}}
	if got := msddl.CreateIndex(tb, ix); !strings.Contains(got, "INCLUDE ([total])") {
		t.Errorf("the covering columns were dropped:\n  %s", got)
	}
}

// A MAX column cannot be an index KEY — no bound, and the key limit is 1700
// bytes — but it CAN be carried as an INCLUDE, which is the fix worth naming
// because SQL Server has one and MySQL does not.
func TestIndexingAMaxColumnIsRefusedWithTheFix(t *testing.T) {
	tb := &schema.Table{Name: "docs", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true),
		col("body", schema.Type{Name: schema.TypeText}, true),
	}, PrimaryKey: []string{"id"},
		Indexes: []*schema.Index{{Name: "ix_docs_body",
			Columns: []schema.IndexColumn{{Name: "body"}}}}}

	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil {
		t.Fatal("indexing an NVARCHAR(MAX) key was accepted")
	}
	if !strings.Contains(err.Error(), "INCLUDE") {
		t.Errorf("the refusal does not name the fix this server has: %s", err)
	}
}

// uuidv7() has no faithful spelling. NEWID() is a version 4 uuid and
// NEWSEQUENTIALID() derives from the server's MAC; substituting either keeps
// the type and loses the index locality the model asked for.
func TestUUIDv7IsRefusedRatherThanApproximated(t *testing.T) {
	c := col("id", schema.Type{Name: schema.TypeUUID}, true)
	c.Default = "uuidv7()"
	tb := &schema.Table{Name: "t", Columns: []*schema.Column{c}, PrimaryKey: []string{"id"}}

	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil {
		t.Fatal("uuidv7() was accepted; NEWID() is version 4")
	}
	for _, want := range []string{"NEWID()", "NEWSEQUENTIALID()", "GenRandomUUID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %s", want, err)
		}
	}

	// gen_random_uuid() DOES cross, which is the difference from MySQL that
	// lets the key stay the database's job here.
	c.Default = "gen_random_uuid()"
	def, err := msddl.ColumnDef("t", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "DEFAULT NEWID()") {
		t.Errorf("gen_random_uuid() did not become a server-side default:\n  %s", def)
	}
}

// An arc's CHECK is storm's own, and its PostgreSQL spelling casts each arm
// with ::int, which parses nowhere else. SQL Server has no boolean-to-int
// coercion either — a predicate is not a value — so each arm is a CASE.
func TestArcCheckIsRespelled(t *testing.T) {
	tb := &schema.Table{Name: "events", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true),
		col("order_id", schema.Type{Name: schema.TypeUUID}, false),
		col("invoice_id", schema.Type{Name: schema.TypeUUID}, false),
	}, PrimaryKey: []string{"id"},
		Checks: []*schema.Check{{Name: "ck_events_subject",
			Arc: []string{"order_id", "invoice_id"}}}}

	got, err := msddl.CreateTable(tb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "::int") {
		t.Errorf("a PostgreSQL cast survived into SQL Server DDL:\n  %s", got)
	}
	want := "CASE WHEN [order_id] IS NOT NULL THEN 1 ELSE 0 END + " +
		"CASE WHEN [invoice_id] IS NOT NULL THEN 1 ELSE 0 END = 1"
	if !strings.Contains(got, want) {
		t.Errorf("the arc check is not the exclusive-arm form:\n  %s\nwant it to contain\n  %s",
			got, want)
	}
}

// RESTRICT has no keyword here and NO ACTION is what it means. The distinction
// PostgreSQL draws between them is not observable through a constraint storm
// generates, which is never DEFERRABLE.
func TestRestrictBecomesNoAction(t *testing.T) {
	tb := &schema.Table{Name: "lines"}
	fk := &schema.ForeignKey{Name: "fk_lines_order", Columns: []string{"order_id"},
		RefTable: "orders", RefColumns: []string{"id"}, OnDelete: schema.Restrict}
	got := msddl.AddForeignKey(tb, fk)
	if strings.Contains(got, "RESTRICT") {
		t.Errorf("RESTRICT reached SQL Server, which has no such keyword:\n  %s", got)
	}
	if !strings.Contains(got, "ON DELETE NO ACTION") {
		t.Errorf("the action was dropped rather than translated:\n  %s", got)
	}
}

// Identifiers are bracketed, and a bracket inside one is doubled. Double quotes
// work only under QUOTED_IDENTIFIER ON, which is a per-connection setting a
// library may not assume about someone else's server.
func TestIdentIsBracketed(t *testing.T) {
	if got := msddl.Ident("order"); got != "[order]" {
		t.Errorf("Ident(order) = %s", got)
	}
	if got := msddl.Ident("we]rd"); got != "[we]]rd]" {
		t.Errorf("a bracket inside a name was not doubled: %s", got)
	}
}

// A table with no primary key is a heap: nothing can reference it, and a
// filtered index has no key to be built against.
func TestHeapIsRefused(t *testing.T) {
	tb := &schema.Table{Name: "events", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true),
	}}
	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil || !strings.Contains(err.Error(), "heap") {
		t.Errorf("a table with no primary key was accepted: %v", err)
	}
}

// An enum has no native type here, so it is a width and a constraint — the same
// accepted values MySQL's native ENUM has, stored as the label rather than an
// ordinal.
func TestEnumBecomesACheckedColumn(t *testing.T) {
	e := &schema.Enum{Name: "order_status", Labels: []string{"new", "paid", "cancelled"}}
	if got := msddl.TypeEnum(e); got != "NVARCHAR(9)" {
		t.Errorf("TypeEnum = %s, want NVARCHAR(9) — the widest label", got)
	}
	got := msddl.EnumCheck("status", e)
	for _, l := range e.Labels {
		if !strings.Contains(got, "'"+l+"'") {
			t.Errorf("the check does not accept %q: %s", l, got)
		}
	}
}

// Every problem in one pass, not the first: finding them one deploy at a time
// is the failure mode this replaces.
func TestCheckReportsEveryProblem(t *testing.T) {
	tb := &schema.Table{Name: "t", Columns: []*schema.Column{
		col("a", schema.Type{Name: schema.TypeInterval}, true),
		col("b", schema.Type{Name: schema.TypeInet}, true),
		col("c", schema.Type{Name: schema.TypeHstore}, true),
	}}
	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil {
		t.Fatal("three impossible columns were accepted")
	}
	for _, want := range []string{"INTERVAL", "network address", "hstore", "heap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report stops before %s:\n%s", want, err)
		}
	}
}

// The index access methods PostgreSQL has and SQL Server does not, each naming
// what to use instead rather than just saying no.
func TestIndexMethodsThatDoNotCross(t *testing.T) {
	for _, m := range []struct{ method, mentions string }{
		{"gin", "CONTAINS()"},
		{"gist", "spatial"},
		{"spgist", "spatial"},
		{"brin", "columnstore"},
		{"fulltext", "CREATE FULLTEXT CATALOG"},
		{"hash", "memory-optimized"},
		{"bloom", "access method"},
	} {
		tb := &schema.Table{Name: "t", Columns: []*schema.Column{
			col("id", schema.Type{Name: schema.TypeUUID}, true),
			col("a", schema.Type{Name: schema.TypeVarchar, Size: 20}, true),
		}, PrimaryKey: []string{"id"},
			Indexes: []*schema.Index{{Name: "ix", Method: m.method,
				Columns: []schema.IndexColumn{{Name: "a"}}}}}
		err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
		if err == nil {
			t.Errorf("%s was accepted", m.method)
			continue
		}
		if !strings.Contains(err.Error(), m.mentions) {
			t.Errorf("%s: the refusal does not name the alternative %q:\n%s",
				m.method, m.mentions, err)
		}
	}

	// btree and the empty default are the ones that cross.
	for _, m := range []string{"", "btree"} {
		tb := &schema.Table{Name: "t", Columns: []*schema.Column{
			col("id", schema.Type{Name: schema.TypeUUID}, true),
			col("a", schema.Type{Name: schema.TypeVarchar, Size: 20}, true),
		}, PrimaryKey: []string{"id"},
			Indexes: []*schema.Index{{Name: "ix", Method: m,
				Columns: []schema.IndexColumn{{Name: "a"}}}}}
		if err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}}); err != nil {
			t.Errorf("method %q was refused: %v", m, err)
		}
	}
}

// The index options with no SQL Server equivalent, and the ones that do cross.
func TestIndexOptions(t *testing.T) {
	base := func(ix *schema.Index) *schema.Schema {
		return &schema.Schema{Tables: []*schema.Table{{Name: "t",
			Columns: []*schema.Column{
				col("id", schema.Type{Name: schema.TypeUUID}, true),
				col("a", schema.Type{Name: schema.TypeVarchar, Size: 20}, true),
			},
			PrimaryKey: []string{"id"}, Indexes: []*schema.Index{ix}}}}
	}
	for _, c := range []struct {
		ix       *schema.Index
		mentions string
	}{
		{&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "a", OpClass: "text_ops"}}},
			"operator classes"},
		{&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "a", NullsFirst: true}}},
			"sorts NULLs first"},
		{&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "a", Prefix: 10}}},
			"no prefix keys"},
		{&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "lower(a)", Expr: true}}},
			"PERSISTED computed column"},
		{&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "a"}},
			With: []schema.StorageParam{{Name: "fillfactor", Value: "70"}}}, "storage parameter"},
	} {
		err := msddl.Check(base(c.ix))
		if err == nil || !strings.Contains(err.Error(), c.mentions) {
			t.Errorf("expected a refusal mentioning %q, got: %v", c.mentions, err)
		}
	}

	// DESC keys DO cross, and so does an explicit WHERE.
	got := msddl.CreateIndex(&schema.Table{Name: "t"},
		&schema.Index{Name: "ix", Columns: []schema.IndexColumn{{Name: "a", Desc: true}},
			Where: "a IS NOT NULL"})
	if !strings.Contains(got, "[a] DESC") || !strings.Contains(got, "WHERE a IS NOT NULL") {
		t.Errorf("a descending filtered index lost something:\n  %s", got)
	}

	// An unnamed index is named after the table and its keys, so two models
	// cannot collide and a migration can find it again.
	unnamed := msddl.CreateIndex(&schema.Table{Name: "orders"},
		&schema.Index{Columns: []schema.IndexColumn{{Name: "status"}}})
	if !strings.Contains(unnamed, "[ix_orders_status]") {
		t.Errorf("an unnamed index got no derived name:\n  %s", unnamed)
	}
}

// EXCLUDE constraints have no equivalent, and the overlap they prevent becomes
// a race the application cannot win — so this is a refusal, not a widening.
func TestExcludeIsRefused(t *testing.T) {
	tb := &schema.Table{Name: "bookings", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true)},
		PrimaryKey: []string{"id"},
		Excludes:   []*schema.Exclude{{Name: "no_overlap"}}}
	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil || !strings.Contains(err.Error(), "race") {
		t.Errorf("an EXCLUDE constraint was accepted: %v", err)
	}
}

// An enum column whose enum is not declared is a dangling reference, and the
// DDL would name a type that does not exist.
func TestUndeclaredEnumIsRefused(t *testing.T) {
	c := col("status", schema.Type{Name: "order_status", Enum: true}, true)
	tb := &schema.Table{Name: "orders", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true), c},
		PrimaryKey: []string{"id"}}
	err := msddl.Check(&schema.Schema{Tables: []*schema.Table{tb}})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("a dangling enum reference was accepted: %v", err)
	}
	// TypeSQL sends the caller to TypeEnum rather than guessing a width.
	if _, err := msddl.TypeSQL("orders", c); err == nil ||
		!strings.Contains(err.Error(), "TypeEnum") {
		t.Errorf("TypeSQL on an enum does not name TypeEnum: %v", err)
	}
}

// The whole schema, in the order a server can apply it: tables, then indexes,
// then the foreign keys that reference them.
func TestCreateOrdersTablesBeforeTheirReferences(t *testing.T) {
	orgs := &schema.Table{Name: "orgs", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true)},
		PrimaryKey: []string{"id"},
		Indexes: []*schema.Index{{Name: "ix_orgs_id",
			Columns: []schema.IndexColumn{{Name: "id"}}}}}
	members := &schema.Table{Name: "members", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true),
		col("org_id", schema.Type{Name: schema.TypeUUID}, true)},
		PrimaryKey: []string{"id"},
		ForeignKeys: []*schema.ForeignKey{{Name: "fk_members_org", Columns: []string{"org_id"},
			RefTable: "orgs", RefColumns: []string{"id"}}}}

	got, err := msddl.Create(&schema.Schema{Tables: []*schema.Table{orgs, members}})
	if err != nil {
		t.Fatal(err)
	}
	table := strings.Index(got, "CREATE TABLE [members]")
	index := strings.Index(got, "CREATE INDEX")
	fk := strings.Index(got, "ADD CONSTRAINT [fk_members_org]")
	if table < 0 || index < 0 || fk < 0 || !(table < index && index < fk) {
		t.Errorf("the statements are not in an appliable order:\n%s", got)
	}

	// A schema with an impossible column produces no DDL at all, rather than
	// DDL that fails halfway through applying.
	bad := &schema.Schema{Tables: []*schema.Table{{Name: "t",
		Columns:    []*schema.Column{col("a", schema.Type{Name: schema.TypeHstore}, true)},
		PrimaryKey: []string{"a"}}}}
	if out, err := msddl.Create(bad); err == nil || out != "" {
		t.Errorf("a partial schema was emitted: %q, %v", out, err)
	}
}

// A computed column names no type and takes PERSISTED — which, unlike MariaDB,
// accepts a nullability clause after it.
func TestComputedColumnIsPersisted(t *testing.T) {
	c := col("upper_name", schema.Type{Name: schema.TypeVarchar, Size: 200}, true)
	c.Generated = "UPPER([name])"
	got, err := msddl.ColumnDef("t", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[upper_name] AS (UPPER([name])) PERSISTED NOT NULL" {
		t.Errorf("got %s", got)
	}
}

// IDENTITY(1,1) rather than a sequence: no separate object, and OUTPUT hands
// the assigned value back so nothing has to ask afterwards.
func TestIdentityColumn(t *testing.T) {
	c := col("n", schema.Type{Name: schema.TypeInt8}, true)
	c.Identity = true
	got, err := msddl.ColumnDef("t", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "IDENTITY(1,1)") {
		t.Errorf("got %s", got)
	}
}

// now() is the server's clock at a resolution two rows in one statement can be
// told apart by, and carrying the offset.
func TestNowDefaultIsOffsetAware(t *testing.T) {
	c := col("created_at", schema.Type{Name: schema.TypeTimestamptz}, true)
	c.Default = "now()"
	got, err := msddl.ColumnDef("t", c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "DEFAULT SYSDATETIMEOFFSET()") {
		t.Errorf("got %s", got)
	}
	// A default storm did not generate is passed through: it is the model's own
	// SQL, and rewriting somebody's expression is guesswork.
	c.Default = "42"
	if got, _ := msddl.ColumnDef("t", c, nil); !strings.Contains(got, "DEFAULT 42") {
		t.Errorf("a literal default was dropped: %s", got)
	}
}

// An enum column renders, and the CHECK that makes it an enum comes with it.
//
// This is the gap the two halves of the package disagreed about: Check accepted
// a model with an enum and Create then refused it, because TypeSQL has no
// labels and nothing supplied them. A check that says a model ports and a
// generator that says it does not is the one disagreement this package exists
// to prevent.
func TestEnumColumnRendersRatherThanRefusing(t *testing.T) {
	e := &schema.Enum{Name: "order_status", Labels: []string{"new", "paid", "cancelled"}}
	c := col("status", schema.Type{Name: "order_status", Enum: true}, true)
	tb := &schema.Table{Name: "orders", Columns: []*schema.Column{
		col("id", schema.Type{Name: schema.TypeUUID}, true), c,
	}, PrimaryKey: []string{"id"}}
	s := &schema.Schema{Tables: []*schema.Table{tb}, Enums: []*schema.Enum{e}}

	if err := msddl.Check(s); err != nil {
		t.Fatalf("Check refused a model with a declared enum: %v", err)
	}
	got, err := msddl.Create(s)
	if err != nil {
		t.Fatalf("Check accepted this model and Create refused it: %v", err)
	}
	// The width is the widest label: there is no enum TYPE here, so the column
	// is a string and the declaration is what bounds it.
	if !strings.Contains(got, "[status] NVARCHAR(9) NOT NULL") {
		t.Errorf("the column is not the enum's width:\n%s", got)
	}
	// And the CHECK is what makes it an enum rather than any string at all.
	for _, l := range e.Labels {
		if !strings.Contains(got, "'"+l+"'") {
			t.Errorf("the constraint does not accept %q:\n%s", l, got)
		}
	}
	if !strings.Contains(got, "CONSTRAINT [ck_orders_status] CHECK") {
		t.Errorf("no constraint, so the column takes any string:\n%s", got)
	}

	// Without the labels the refusal is still there, and it names the schema
	// rather than telling the caller to call a function they cannot reach.
	if _, err := msddl.CreateTable(tb, nil); err == nil {
		t.Error("a table with an enum rendered without the labels")
	}
}
