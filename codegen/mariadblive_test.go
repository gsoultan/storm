package codegen_test

import (
	"context"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/runtime/mydrv"
	"github.com/gsoultan/storm/schema"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The end-to-end that has never happened: a GENERATED storm package executing
// against a real MySQL-family server, through storm's own adapter.
//
// Everything before this proved a piece — the SQL PREPAREs, mydec decodes the
// bytes, the adapter satisfies the port. None of them proved the pieces fit,
// and "each part works" is the claim that has been wrong twice in M9 already.
//
// Run twice, against BOTH engines, each with the dialect it was generated for.
// One test reading one address could not tell which server it had reached, and
// the MariaDB-dialect package contains SQL that MySQL rejects.
func TestGeneratedMariaDBPackageRunsAgainstAServer(t *testing.T) {
	runGeneratedLive(t, "mariadb", "STORM_MARIADB_ADDR",
		[]string{"TestInsertSelectUpdateDelete", "TestInsertReturnsTheRow"})
}

func TestGeneratedMySQLPackageRunsAgainstAServer(t *testing.T) {
	runGeneratedLive(t, "mysql", "STORM_MYSQL_ADDR",
		[]string{"TestInsertSelectUpdateDelete"})
}

func runGeneratedLive(t *testing.T, dialect, addrVar string, want []string) {
	if os.Getenv(addrVar) == "" {
		t.Skip(addrVar + " unset")
	}
	sweepGenerated(t, "mdlive")
	// NOT buildSoftDelete's model: its t.Unique is scoped to the live rows,
	// which is a PARTIAL unique index, and MySQL has none — myddl.Check
	// refuses it, correctly. On this engine a soft-delete table can have
	// uniqueness across ALL rows or none, and that is an engine limit rather
	// than a storm gap. See TestSoftDeleteScopedUniqueDoesNotPort.
	s, err := storm.Build(&mdUser{})
	if err != nil {
		t.Fatal(err)
	}
	root, err2 := filepath.Abs("..")
	if err2 != nil {
		t.Fatal(err2)
	}
	base := "mdlive" + dialect + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := pkgFor(t, s, dir, dialect)
	if err != nil {
		t.Fatalf("generating for %s: %v", dialect, err)
	}
	var pkgDir string
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, ".gen.go") && strings.Contains(rel, "/") {
			pkgDir = filepath.Dir(full)
		}
	}
	if pkgDir == "" {
		t.Fatal("no per-table package generated")
	}
	pkg := filepath.Base(pkgDir)
	returning := ""
	if dialect == "mariadb" {
		returning = mariadbReturningSrc
	}
	src := strings.ReplaceAll(mysqlLiveSrc, "RETURNINGTEST", returning)
	src = strings.ReplaceAll(src, "PKG", pkg)
	src = strings.ReplaceAll(src, "ADDRVAR", addrVar)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "120s", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), addrVar+"="+os.Getenv(addrVar))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that skipped is a green test that proved nothing — the trap
	// the soft-delete live test already fell into once.
	for _, name := range want {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

// sweepGenerated removes packages an earlier run left behind.
//
// t.Cleanup does not run when a test is KILLED, and a live test against a
// hung server is exactly the case that gets killed. The leftovers then join
// ./... — they are inside the module — and break gofmt and the package count
// for every later run, which reads as a failure in whatever ran next.
func sweepGenerated(t *testing.T, prefix string) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	old, err := filepath.Glob(filepath.Join(root, "internal", prefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range old {
		if err := os.RemoveAll(d); err != nil {
			t.Fatalf("removing a leftover generated package: %v", err)
		}
	}
}

func pkgFor(t *testing.T, s *schema.Schema, dir, dialect string) (map[string][]byte, error) {
	t.Helper()
	return codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm", Dialect: dialectFor(dialect),
	})
}

func dialectFor(name string) codegen.Dialect {
	switch name {
	case "mysql":
		return codegen.DialectMySQL
	case "mariadb":
		return codegen.DialectMariaDB
	case "mssql":
		return codegen.DialectMSSQL
	}
	return codegen.DialectPostgres
}

// mdUser is a soft-delete model that PORTS: the email is sized so it is a
// VARCHAR rather than LONGTEXT, and its uniqueness spans deleted rows, because
// the live-scoped form is a partial index MySQL cannot express.
type mdUser struct {
	storm.Model
	Email     string
	Name      string
	DeletedAt *time.Time
}

func (u *mdUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	t.UniqueAcrossDeleted(&u.Email)
}

// The limit, stated as a test so it is not rediscovered. storm scopes a
// soft-delete table's uniqueness to the live rows with a PARTIAL unique index,
// which is the only way PostgreSQL can say "unique among the rows that are
// alive" — and MySQL has no partial index at all. myddl.Check refuses it, and
// the message says the rows it excludes would be indexed too.
//
// So on this engine a soft-delete table has uniqueness over EVERY row or none.
// That is a real behavioural difference for an adopter: a deleted row keeps its
// email forever, where on PostgreSQL the address becomes claimable again.
func TestSoftDeleteScopedUniqueDoesNotPortToMySQL(t *testing.T) {
	s := buildSoftDelete(t)
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("a live-scoped unique index ported to MySQL; it has no partial index")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("the refusal does not name the reason:\n%v", err)
	}
}

// The sixth MariaDB divergence, found by the end-to-end.
//
// MySQL 8 accepts a generated column declared `... STORED NOT NULL`. MariaDB
// 11.4 rejects it: its grammar allows no nullability clause after
// VIRTUAL/PERSISTENT/STORED, and derives nullability from the expression
// instead. myddl.CreateFor drops the clause for MariaDB and keeps it for MySQL.
//
// Asserted against BOTH servers, because the whole failure was that one of them
// accepted what the other would not, and a golden test cannot tell which.
func TestGeneratedColumnDDLAppliesToBothEngines(t *testing.T) {
	s, err := storm.Build(&genUser{})
	if err != nil {
		t.Fatal(err)
	}
	my, err := myddl.CreateFor(s, myddl.MySQL)
	if err != nil {
		t.Fatal(err)
	}
	md, err := myddl.CreateFor(s, myddl.MariaDB)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(my, "STORED NOT NULL") {
		t.Errorf("the MySQL form lost its NOT NULL, which MySQL does enforce:\n%s", my)
	}
	if strings.Contains(md, "STORED NOT NULL") {
		t.Errorf("the MariaDB form still carries NOT NULL, which MariaDB rejects:\n%s", md)
	}

	applyDDL(t, "STORM_MYSQL_ADDR", my)
	applyDDL(t, "STORM_MARIADB_ADDR", md)
}

// applyDDL runs a schema against a server through storm's own adapter — the
// point being that the server accepts it, which no golden test can establish.
func applyDDL(t *testing.T, addrVar, ddl string) {
	t.Helper()
	addr := os.Getenv(addrVar)
	if addr == "" {
		t.Skip(addrVar + " unset")
	}
	ctx := context.Background()
	c, err := mydrv.Open(ctx, mydrv.Config{
		Addr: addr, User: "root", Password: "storm", Database: "storm",
		AllowCleartextPasswordOverPlaintext: true,
	})
	if err != nil {
		t.Fatalf("%s: %v", addrVar, err)
	}
	defer c.Close()
	if _, err := c.Exec(ctx, "DROP TABLE IF EXISTS `gen_users`", nil); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(ddl, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := c.Exec(ctx, stmt, nil); err != nil {
			t.Fatalf("%s rejected:\n%s\n%v", addrVar, stmt, err)
		}
	}
	t.Cleanup(func() { _, _ = c.Exec(ctx, "DROP TABLE IF EXISTS `gen_users`", nil) })
}

// A model with a NOT NULL generated column, which is the only shape that shows
// the divergence.
type genUser struct {
	storm.Model
	First string
	Last  string
	Full  string
}

func (u *genUser) Schema(t *storm.Table) {
	t.Col(&u.First).Size(60)
	t.Col(&u.Last).Size(60)
	t.Col(&u.Full).Size(121).Generated(storm.RawSQL("concat(`first`,' ',`last`)"))
}

// Two tables, a foreign key and a named plan, against a real server.
//
// The single-table end-to-end proves CRUD. It cannot prove the BATCH LOADER,
// which is the construct M9's exit gate names and the one genuinely different
// here: PostgreSQL unnests an array, MySQL reaches the same answer through
// JSON_TABLE and MariaDB through a window. Those forms PREPARE in the shell
// gates; nothing had checked the rows they return, or which parent each child
// was attached to.
func TestGeneratedRelationsRunAgainstMySQL(t *testing.T) {
	runRelationsLive(t, "mysql", "STORM_MYSQL_ADDR", "MySQL")
}

func TestGeneratedRelationsRunAgainstMariaDB(t *testing.T) {
	runRelationsLive(t, "mariadb", "STORM_MARIADB_ADDR", "MariaDB")
}

func runRelationsLive(t *testing.T, dialect, addrVar, ddlTarget string) {
	if os.Getenv(addrVar) == "" {
		t.Skip(addrVar + " unset")
	}
	sweepGenerated(t, "mdrel")
	s, err := storm.Build(&mdAuthor{}, &mdPost{}, &mdNode{}, &mdTag{}, &mdAttachment{}, &mdWide{}, &mdFollow{}, &mdEvent{}, mdNames)
	if err != nil {
		t.Fatal(err)
	}
	root, err2 := filepath.Abs("..")
	if err2 != nil {
		t.Fatal(err2)
	}
	base := "mdrel" + dialect + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	// Package and PackageImport are what make codegen emit the CONTEXT
	// package. Without them only the per-table packages appear, and a plan
	// spans tables so it lives in neither.
	pkg := filepath.Base(dir)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm", Dialect: dialectFor(dialect),
		Package:       pkg,
		PackageImport: "github.com/gsoultan/storm/internal/" + base,
	})
	if err != nil {
		t.Fatalf("generating for %s: %v", dialect, err)
	}
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.ReplaceAll(relationsLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "ADDRVAR", addrVar)
	src = strings.ReplaceAll(src, "TARGET", ddlTarget)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "120s", "./internal/"+base+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), addrVar+"="+os.Getenv(addrVar))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	for _, name := range []string{
		"TestPlanLoadsEveryChildInTwoRoundTrips",
		"TestChildTopUsesTheBatchLoader",
		"TestChildQueryFiltersByItsParent",
		"TestKeysetPagingOverTheParents",
		"TestDeclaredJoinReturnsBothSidesRows",
		"TestDeclaredAggregateGroupsAndFilters",
		"TestSemiJoinDoesNotMultiplyTheParent",
		"TestUnionMergesBothTablesInOneOrder",
		"TestRowLockingInsideATransaction",
		"TestRecursiveDescendsAndAscends",
		"TestRecursiveTerminatesOnACycle",
		"TestProjectionReadsItsSubset",
		"TestUnitFlushesInForeignKeyOrder",
		"TestSoftDeleteReachesEveryCrossTableRead",
		"TestManyToManyLoadsBothDirections",
		"TestArcLoadsEveryVariantAndEnforcesExactlyOne",
		"TestAKeyIsGeneratedWhenTheCallerDoesNotSetOne",
		"TestUpdateWritesOnlyWhatWasSet",
		"TestUpdateSkipsASoftDeletedRow",
		"TestInsertAllLoadsEveryRow",
		"TestAnyOfBracketsItsConjunctions",
		"TestOffsetAndUnordered",
		"TestEveryColumnTypeRoundTripsThroughGeneratedCode",
		"TestJSONPredicates",
		"TestManyToManyThroughAPayloadModel",
		"TestSelfReferentialManyToMany",
		"TestAnyRefRoundTripsAndIsIndexable",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

// The two-table model the relation tests generate from. Declared here so the
// harness can Build it; the generated package redeclares it to Build the DDL,
// which is the same trick the single-table test plays.
type mdAuthor struct {
	storm.Model
	Name    string
	Posts   []mdPost
	Follows []mdTag
}

func (a *mdAuthor) Schema(t *storm.Table) {
	t.Col(&a.Name).Size(80)
	t.Through(&a.Follows, mdFollow{})
}

// A declared column subset, which has its own scan path and its own statement.
func (a *mdAuthor) Projections(p *storm.Projections) { p.Named("Card", &a.Name) }
func (a *mdAuthor) Plans(p *storm.Plans)             { p.Named("Feed").With(&a.Posts) }

type mdPost struct {
	storm.Model
	Title     string
	Views     int64
	DeletedAt *time.Time
	Author    mdAuthor
	Tags      []mdTag
}

func (p *mdPost) Schema(t *storm.Table) {
	// Soft delete on the CHILD, so the cross-table reads have something to
	// exclude: a plan, a join, an aggregate and a union each have to carry the
	// predicate to the right alias, which is a different problem from carrying
	// it on a single-table read.
	t.SoftDelete(&p.DeletedAt)
	t.Col(&p.Title).Size(120)
}

// A declared join, aggregate and union, so the live test reaches the
// constructs that only ever had their SQL TEXT asserted. Each has a MySQL
// lowering of its own — a join's ON clause, an aggregate's GROUP BY and
// HAVING, a union's bare placeholders — and none had run against a server
// through generated code.
func (p *mdPost) Joins(j *storm.Joins) {
	var a mdAuthor
	j.Named("WithAuthor").
		Inner(&a, &p.Author).
		Take(&p.Title, "Title").
		Take(&p.Views, "Views").
		Take(&a.Name, "AuthorName")
}

func (p *mdPost) Aggregates(a *storm.Aggregates) {
	byAuthor := a.Named("ByAuthor")
	// Grouped by the relation, which is the foreign-key column storm derives
	// from it — there is no AuthorID field to point at.
	byAuthor.By(&p.Author)
	n := byAuthor.Count("Posts")
	byAuthor.Sum(&p.Views, "Views")
	byAuthor.Max(&p.Views, "TopViews")
	byAuthor.Having(a.Gt(n, 0))
}

// A self-referential table, which is what generates WITH RECURSIVE. It takes
// its roots as a bound key list — the same JSON-document path that broke every
// fetch plan — and its cycle guard is the one construct that differs most
// between the engines: PostgreSQL accumulates visited keys in an ARRAY, MySQL
// has none and uses a HEX string with FIND_IN_SET.
// Every scalar type that ports, so the GENERATED SCANNER for each one is
// exercised against real server bytes. The driver's own round trip covers the
// decoders; this covers the code that calls them, which is a different path and
// the one every read goes through.
type mdWide struct {
	storm.Model
	Flag    bool
	Small   int16
	Medium  int32
	Big     int64
	Single  float32
	Double  float64
	Text    string
	Blob    []byte
	Stamp   time.Time
	Day     time.Time
	Clock   storm.TimeOfDay
	Money   storm.Decimal
	Doc     storm.JSON
	OptText *string
	OptBig  *int64
	OptDay  *time.Time
}

func (w *mdWide) Schema(t *storm.Table) {
	t.Col(&w.Text).Size(80)
	t.Col(&w.Day).Date()
	t.Col(&w.Money).Numeric(18, 6)
}

// A join model with a PAYLOAD: the row records something the generated join
// table has nowhere to put. t.Through names it, and its loader is a different
// shape from the implicit many-to-many's.
type mdFollow struct {
	Author mdAuthor
	Tag    mdTag
	Since  time.Time
	Reason string
}

func (f *mdFollow) Schema(t *storm.Table) {
	t.PrimaryKey(&f.Author, &f.Tag)
	t.Col(&f.Author).OnDelete(storm.Cascade)
	t.Col(&f.Tag).OnDelete(storm.Cascade)
	t.Col(&f.Reason).Size(40)
}

// The discriminator form of a polymorphic reference: a table name and an id,
// with no foreign key, acknowledged where a reviewer sees it.
type mdEvent struct {
	storm.Model
	Kind    string
	Subject storm.AnyRef
}

func (e *mdEvent) Schema(t *storm.Table) {
	t.Col(&e.Kind).Size(40)
	t.Col(&e.Subject).AcknowledgeNoFK("events outlive the rows they describe, by design")
}

type mdNode struct {
	storm.Model
	Name     string
	Parent   *mdNode
	Children []mdNode
}

func (n *mdNode) Schema(t *storm.Table) {
	t.Col(&n.Name).Size(60)
	t.Col(&n.Parent).OnDelete(storm.Cascade)
}

// The implicit MANY-TO-MANY: a slice on both sides and storm generates the join
// table nobody declared. Its loader is a two-hop read no other test reaches.
type mdTag struct {
	storm.Model
	Label string
	Posts []mdPost
	// A SELF-referential many-to-many: both sides are the same table, so the
	// join table's two columns cannot be told apart by their type. On mdTag
	// rather than mdNode, because mdNode also has a self FK and a table with
	// both cannot say which relation a slice means.
	Similar []mdTag
}

func (g *mdTag) Schema(t *storm.Table) { t.Col(&g.Label).Size(40) }

// The polymorphic ARC: exactly one of the variants is set, enforced by the
// database rather than by the caller. Its loader batches per variant, and the
// CHECK that enforces exactly-one is a construct MySQL only gained in 8.0.16.
type mdAttachment struct {
	storm.Model
	Filename string
	Subject  storm.OneOf2[mdAuthor, mdPost]
}

func (a *mdAttachment) Schema(t *storm.Table) { t.Col(&a.Filename).Size(120) }

// A union has no driving table, so it hangs off the schema rather than off
// either model (ADR-0008).
var mdNames = storm.Union("Names", func(u *storm.UnionSpec) {
	var a mdAuthor
	authors := u.From(&a)
	authors.Take(&a.Name, "Text")
	authors.Const("Kind", "author")

	var p mdPost
	posts := u.From(&p)
	posts.Take(&p.Title, "Text")
	posts.Const("Kind", "post")

	u.OrderAsc("Text")
})
