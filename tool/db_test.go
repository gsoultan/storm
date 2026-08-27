package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/internal/testmodel"
	"github.com/jackc/pgx/v5"
)

// The database-backed commands, against a real one. Without these the CLI's
// tested half is the half that cannot lose data.

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("STORM_DSN")
	if d == "" {
		t.Skip("STORM_DSN unset")
	}
	return d
}

// namespace gives each test its own schema, so they can run shuffled and in
// parallel without diffing each other's tables.
func namespace(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+name+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c2, err := pgx.Connect(context.Background(), dsn(t))
		if err != nil {
			return
		}
		defer c2.Close(context.Background())
		_, _ = c2.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+name+" CASCADE")
	})
	return name
}

// An empty database has every table pending; applying the diff makes verify
// pass. That round trip is the whole migration workflow in one test.
func TestCLI_DiffThenVerify(t *testing.T) {
	withModels(t, testmodel.All())
	ns := namespace(t, "cli_diff")
	out := t.TempDir()

	// Against an empty namespace the model has drifted, and verify must say so
	// rather than exiting zero on a database with no tables at all.
	if err := run([]string{"verify", "-dsn", dsn(t), "-schema", ns}); err == nil {
		t.Fatal("verify against an empty database must report drift")
	}

	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(out, "*.sql"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one migration file, got %v (%v)", files, err)
	}
	sql, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), `CREATE TABLE "users"`) {
		t.Errorf("the migration does not create users:\n%s", sql)
	}

	// Apply it, then the model and the database agree.
	applySQL(t, ns, string(sql))
	if err := run([]string{"verify", "-dsn", dsn(t), "-schema", ns}); err != nil {
		t.Fatalf("verify after applying its own migration: %v", err)
	}

	// And a second diff is empty — migrations converge.
	out2 := t.TempDir()
	if err := run([]string{"diff", "again", "-dsn", dsn(t), "-schema", ns, "-out", out2}); err != nil {
		t.Fatal(err)
	}
	if f, _ := filepath.Glob(filepath.Join(out2, "*.sql")); len(f) != 0 {
		t.Errorf("a second diff produced %v — migrations do not converge", f)
	}
}

// import must reproduce a model from a live database.
func TestCLI_Import(t *testing.T) {
	withModels(t, testmodel.All())
	ns := namespace(t, "cli_import")

	out := t.TempDir()
	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "*.sql"))
	sql, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	applySQL(t, ns, string(sql))

	got := captureStdout(t, func() {
		if err := run([]string{"import", "-dsn", dsn(t), "-schema", ns}); err != nil {
			t.Fatal(err)
		}
	})
	// gofmt aligns struct fields, so compare on collapsed whitespace rather
	// than exact spacing — otherwise this breaks whenever a longer field name
	// is added to an unrelated table.
	flat := strings.Join(strings.Fields(got), " ")
	for _, want := range []string{
		"type User struct", "type Org struct",
		"storm.Model",      // the Model triple is embedded, as a human would write it
		"Email string",     // a plain column
		"Status Status",    // an enum column keeps its own type, not string
		"Org Org",          // a foreign key came back as a relation, not a uuid
		"Parent *Org",      // a nullable self-reference is a pointer
		"func All() []any", // ready to pass to storm.Build
		"NOT CARRIED OVER", // and it says what it could not express
		"EnumValues",       // the enum came with its labels
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("imported model is missing %q:\n%s", want, truncate(got))
		}
	}
	if strings.Contains(got, "CREATE TABLE") {
		t.Error("import printed DDL; the point is to produce a MODEL you do not have")
	}
}

// A destructive change must be refused unless it was asked for explicitly. This
// is the one command that can lose data.
func TestCLI_DiffRefusesDestructiveByDefault(t *testing.T) {
	ns := namespace(t, "cli_destructive")
	out := t.TempDir()

	// Build the full schema first...
	withModels(t, testmodel.All())
	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "*.sql"))
	sql, _ := os.ReadFile(files[0])
	applySQL(t, ns, string(sql))

	// ...then put a table in the namespace the model does not have, so the diff
	// has to drop it. A smaller model set would not work: dropping User leaves
	// Org.Users with no target, which is a declaration error rather than a
	// destructive diff.
	applySQL(t, ns, `CREATE TABLE "leftovers" ("id" uuid PRIMARY KEY)`)

	err := run([]string{"diff", "shrink", "-dsn", dsn(t), "-schema", ns, "-out", t.TempDir()})
	if err == nil {
		t.Fatal("a diff that drops tables must be refused without -allow-destructive")
	}
	if !strings.Contains(err.Error(), "destructive") {
		t.Errorf("the error should name the flag, got: %v", err)
	}

	// With the flag it is allowed, and the SQL says what it will do.
	out2 := t.TempDir()
	if err := run([]string{"diff", "shrink", "-dsn", dsn(t), "-schema", ns,
		"-out", out2, "-allow-destructive"}); err != nil {
		t.Fatal(err)
	}
	f2, _ := filepath.Glob(filepath.Join(out2, "*.sql"))
	if len(f2) != 1 {
		t.Fatalf("expected one migration, got %v", f2)
	}
	body, _ := os.ReadFile(f2[0])
	if !strings.Contains(string(body), "DROP TABLE") {
		t.Errorf("a destructive migration should contain the drops:\n%s", truncate(string(body)))
	}
}

func applySQL(t *testing.T, ns, sql string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "SET search_path TO "+ns+"; "+sql); err != nil {
		t.Fatalf("applying the generated migration failed: %v", err)
	}
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "\n…"
	}
	return s
}

// ADR-0001's third verify mode: "changed the model, forgot to run diff" must
// be a CI failure, not a deploy against a schema the code no longer describes.
func TestCLI_VerifyPending(t *testing.T) {
	withModels(t, testmodel.All())
	out := t.TempDir()

	// No migrations at all: everything is pending, and the failure says how to
	// fix itself.
	err := run([]string{"verify", "-pending", "-dsn", dsn(t), "-out", out})
	if err == nil {
		t.Fatal("an empty migrations directory cannot carry the model")
	}
	if !strings.Contains(err.Error(), "storm diff") {
		t.Errorf("the error should name the fix, got: %v", err)
	}

	// Generate the migration; now the set is complete and verify passes.
	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", namespace(t, "cli_pending"), "-out", out}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-pending", "-dsn", dsn(t), "-out", out}); err != nil {
		t.Fatalf("a freshly-generated migration set is reported pending: %v", err)
	}

	// Change the model without a new migration: pending again.
	withModels(t, append(testmodel.All(), &pendingExtra{}))
	if err := run([]string{"verify", "-pending", "-dsn", dsn(t), "-out", out}); err == nil {
		t.Fatal("a new table with no migration must be pending")
	}
}

type pendingExtra struct {
	storm.Model
	Note string
}

// explain's validity half: every statement storm will issue must PLAN, for
// every table and every named plan, which catches a shape PostgreSQL rejects.
// (The performance half needs statistics a CI database does not have; the
// walker's threshold logic is unit-tested against fixtures instead.)
func TestCLI_ExplainPlansEveryStatement(t *testing.T) {
	withModels(t, testmodel.All())
	ns := namespace(t, "cli_explain")
	out := t.TempDir()

	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "*.sql"))
	sql, _ := os.ReadFile(files[0])
	applySQL(t, ns, string(sql))

	stdout := captureStdout(t, func() {
		if err := run([]string{"explain", "-dsn", dsn(t), "-schema", ns}); err != nil {
			t.Fatalf("every generated statement must plan: %v", err)
		}
	})
	for _, want := range []string{
		"users (base read)", "events (base read)",
		"UserFeed → posts", "UserFeed → posts → comments", "OrgTree → users",
		"planned",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain output is missing %q:\n%s", want, stdout)
		}
	}
}

// The generation half of storm.SQL[T]: the statement PREPAREs against the
// MODEL in a scratch schema, the descriptor is matched against the row type,
// and the emitted scanner COMPILES — with the runtime half proven separately
// by a hand-registered scanner in planspike, the P2 split.
func TestCLI_GenerateRawQueries(t *testing.T) {
	withModels(t, testmodel.All())
	prevQ := RawQueries
	// One SQL[T] and one SQLExec: the exec form is validated by the same
	// PREPARE but must emit no scanner.
	RawQueries = append(testmodel.Queries(),
		storm.SQLExec(`DELETE FROM users WHERE id = $1`))
	t.Cleanup(func() { RawQueries = prevQ })

	// Capture the DSN before the no-DSN sub-check clears the env — reading it
	// back afterwards is how this test skipped itself on the first run.
	liveDSN := dsn(t)

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := func(name string) string {
		d := filepath.Join(root, "internal", name+strconv.Itoa(os.Getpid()))
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}

	// Without a DSN the failure names what is needed and why it is small.
	t.Setenv("STORM_DSN", "")
	if err := run([]string{"generate", scratch("rawnodsn")}); err == nil {
		t.Fatal("raw queries without a server must fail")
	} else if !strings.Contains(err.Error(), "an existing schema is not") {
		t.Errorf("the error should say only a server is needed, got: %v", err)
	}

	// With one, generation lands the scanner in the context file — into the
	// repo tree, so the output can be BUILT, which is the bar.
	rel := filepath.Join("internal", "rawgen"+strconv.Itoa(os.Getpid()))
	dir := filepath.Join(root, rel)
	t.Cleanup(func() { os.RemoveAll(dir) })

	if err := run([]string{"generate", dir, "-dsn", liveDSN}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "rawgen"+strconv.Itoa(os.Getpid())+".gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"storm.RegisterScanner(scanEarnerRow)",
		"func scanEarnerRow(rv [][]byte, r *testmodel.EarnerRow, sl *runtime.Slab) error {",
		"r.OrgUsers = runtime.Int8(rv[3])",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the emitted scanner is missing %q", want)
		}
	}
	cmd := exec.Command("go", "build", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated scanner does not compile: %v\n%s", err, out)
	}
}

// The mismatch errors are the feature: each names the column, the type and the
// fix, at BUILD time.
func TestCLI_RawQueryMismatchesFailGeneration(t *testing.T) {
	withModels(t, testmodel.All())
	prevQ := RawQueries
	t.Cleanup(func() { RawQueries = prevQ })

	for _, tc := range []struct {
		name string
		decl storm.RawDecl
		want []string
	}{
		{
			"surplus column",
			storm.SQL[struct{ Email string }](`SELECT email, name FROM users`),
			[]string{`result column 2 "name"`, "has no field", "add `Name string`"},
		},
		{
			"unfed field",
			storm.SQL[struct {
				Email string
				Ghost int64
			}](`SELECT email FROM users`),
			[]string{"Ghost is fed by no result column", `aliased "ghost"`},
		},
		{
			"type mismatch",
			storm.SQL[struct{ Email int64 }](`SELECT email FROM users`),
			[]string{`column 1 "email" is text`, "Email is int64", "cast the column"},
		},
		{
			"does not prepare",
			storm.SQL[struct{ X int64 }](`SELECT nope FROM users`),
			[]string{"does not prepare against the model"},
		},
		{
			"exec that returns rows",
			storm.SQLExec(`SELECT email FROM users`),
			[]string{"storm.SQLExec returns 1 column(s)", "use storm.SQL[T]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RawQueries = []storm.RawDecl{tc.decl}
			root, _ := filepath.Abs("..")
			out := filepath.Join(root, "internal", "rawbad"+strconv.Itoa(os.Getpid()))
			t.Cleanup(func() { os.RemoveAll(out) })
			err := run([]string{"generate", out, "-dsn", dsn(t)})
			if err == nil {
				t.Fatal("generation must fail")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error should contain %q, got:\n%v", w, err)
				}
			}
		})
	}
}

// A model that is a PROJECTION of a bigger schema is the case the first
// adopter actually had: anubis's storm model describes `roles`, while its raw
// declarations call `authorize()` and read `grants` — objects the model
// deliberately does not carry, because migrations are that schema's source of
// truth. Under the default scratch-apply validation every one of those
// statements fails to prepare, which is correct and useless, and it is why
// that adopter could not use this tool at all and wrote its own generator.
//
// So: a raw query against a table the model does not describe must fail with
// -raw-schema model, and succeed against the live database.
func TestCLI_RawSchemaLiveValidatesAgainstTheDatabase(t *testing.T) {
	withModels(t, testmodel.All())
	prevQ := RawQueries
	t.Cleanup(func() { RawQueries = prevQ })

	live := dsn(t)
	c, ctx, done, err := connect(live)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	// An object the MODEL knows nothing about, exactly like a SQL function
	// that lives only in migrations.
	if _, err := c.Exec(ctx, `CREATE TABLE IF NOT EXISTS storm_outside_model (note text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c2, ctx2, done2, err := connect(live)
		if err == nil {
			defer done2()
			_, _ = c2.Exec(ctx2, `DROP TABLE IF EXISTS storm_outside_model`)
		}
	})

	type NoteRow struct{ Note string }
	RawQueries = []storm.RawDecl{
		storm.SQL[NoteRow](`SELECT note FROM storm_outside_model`),
	}

	root, _ := filepath.Abs("..")
	outDir := filepath.Join(root, "internal", "rawlive"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(outDir) })

	// The model cannot vouch for it — and says so naming the mode.
	err = run([]string{"generate", outDir, "-dsn", live})
	if err == nil {
		t.Fatal("a query over a table outside the model must fail the scratch validation")
	}
	if !strings.Contains(err.Error(), "does not prepare against the model schema") {
		t.Errorf("the error should name the mode it validated under, got: %v", err)
	}

	// Against the live database it resolves, and the scanner is emitted.
	if err := run([]string{"generate", outDir, "-dsn", live, "-raw-schema", "live"}); err != nil {
		t.Fatalf("-raw-schema live: %v", err)
	}
	ctxFile := filepath.Join(outDir, filepath.Base(outDir)+".gen.go")
	src, err := os.ReadFile(ctxFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "scanNoteRow") {
		t.Fatal("the live-validated query got no generated scanner")
	}

	// A typo picks nothing silently.
	if err := run([]string{"generate", outDir, "-dsn", live, "-raw-schema", "lvie"}); err == nil {
		t.Fatal("an unknown -raw-schema must be rejected")
	}
}
