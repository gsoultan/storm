package tool

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate/sum"
	"github.com/jackc/pgx/v5"
)

// applyFiles runs migration files the way a runner does: one file per call,
// on a session whose search_path is set apart from the file, so a file holding
// one CREATE INDEX CONCURRENTLY runs outside any transaction.
func applyFiles(t *testing.T, ns string, files ...string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, string(b)); err != nil {
			t.Fatalf("applying %s: %v\n%s", filepath.Base(f), err, b)
		}
	}
}

func globSorted(t *testing.T, pattern string) []string {
	t.Helper()
	f, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f)
	return f
}

func reversed(s []string) []string {
	out := append([]string(nil), s...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// The whole up/down cycle against a server: two versions up, verify, both
// versions down newest first, and the namespace is back where it began —
// including a concurrent index, whose down is concurrent too and pairs with
// its own version.
func TestCLI_DownMigrationsRollBackEveryVersion(t *testing.T) {
	ns := namespace(t, "cli_down")
	out := t.TempDir()

	withModels(t, []any{&noteV1{}})
	if err := run([]string{"diff", "init", "-down", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	applyFiles(t, ns, globSorted(t, filepath.Join(out, "*.up.sql"))...)

	withModels(t, []any{&noteV2{}, &brandNew{}})
	if err := run([]string{"diff", "v2", "-down", "-concurrently", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	ups := globSorted(t, filepath.Join(out, "*.up.sql"))
	downs := globSorted(t, filepath.Join(out, "*.down.sql"))
	if len(ups) != len(downs) {
		t.Fatalf("%d up files and %d down files; every version needs both\nups: %v\ndowns: %v", len(ups), len(downs), ups, downs)
	}
	for i := range ups {
		if strings.TrimSuffix(ups[i], ".up.sql") != strings.TrimSuffix(downs[i], ".down.sql") {
			t.Fatalf("version %d: %s has no matching down (%s)", i, ups[i], downs[i])
		}
	}
	applyFiles(t, ns, ups[1:]...)
	if err := run([]string{"verify", "-dsn", dsn(t), "-schema", ns}); err != nil {
		t.Fatalf("after every up: %v", err)
	}

	// Down every v2 version, newest first: the schema is v1's again.
	applyFiles(t, ns, reversed(downs[1:])...)
	withModels(t, []any{&noteV1{}})
	if err := run([]string{"verify", "-dsn", dsn(t), "-schema", ns}); err != nil {
		t.Fatalf("after rolling v2 back: %v", err)
	}
	// And v1's down empties the namespace.
	applyFiles(t, ns, downs[0])
	var n int
	c, err := pgx.Connect(context.Background(), dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if err := c.QueryRow(context.Background(),
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1", ns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d table(s) left after rolling every version back", n)
	}
	// The down that drops a table says it loses data.
	b, _ := os.ReadFile(downs[0])
	if !strings.Contains(string(b), "DESTRUCTIVE") {
		t.Errorf("a down that drops a table does not say it loses data:\n%s", b)
	}
}

// -replay builds on the migrations directory, not on the dev database: a
// migration nobody has applied locally is still the base, so the next diff
// does not re-create its tables.
func TestCLI_DiffReplayBuildsOnTheDirectoryNotTheDatabase(t *testing.T) {
	ns := namespace(t, "cli_replay")
	out := t.TempDir()

	withModels(t, []any{&noteV1{}})
	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	// Not applied: ns is still empty, as a teammate's dev database is after
	// pulling a branch with a migration in it.
	withModels(t, []any{&noteV1{}, &brandNew{}})
	if err := run([]string{"diff", "add_brand", "-replay", "-dsn", dsn(t), "-out", out}); err != nil {
		t.Fatal(err)
	}
	files := globSorted(t, filepath.Join(out, "*.up.sql"))
	if len(files) != 2 {
		t.Fatalf("want 2 migrations, got %v", files)
	}
	b, _ := os.ReadFile(files[1])
	if strings.Contains(string(b), `CREATE TABLE "notes"`) || !strings.Contains(string(b), `CREATE TABLE "brand_news"`) {
		t.Fatalf("the second migration should create brand_news alone:\n%s", b)
	}
	// And the two together carry the model.
	if err := run([]string{"verify", "-pending", "-dsn", dsn(t), "-out", out}); err != nil {
		t.Fatal(err)
	}
	// With nothing new, -replay says so and writes nothing.
	if err := run([]string{"diff", "nothing", "-replay", "-dsn", dsn(t), "-out", out}); err != nil {
		t.Fatal(err)
	}
	if f := globSorted(t, filepath.Join(out, "*.up.sql")); len(f) != 2 {
		t.Fatalf("an empty replay diff wrote a file: %v", f)
	}
}

// storm.sum: written by diff, and an edit to a summed migration fails every
// command that reads the directory until `storm sum` records it on purpose.
func TestCLI_SumCatchesAnEditedMigration(t *testing.T) {
	ns := namespace(t, "cli_sum")
	out := t.TempDir()
	withModels(t, []any{&noteV1{}})
	if err := run([]string{"diff", "init", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, migratesum.File)); err != nil {
		t.Fatalf("diff did not write %s: %v", migratesum.File, err)
	}
	if err := run([]string{"verify", "-sum", "-out", out}); err != nil {
		t.Fatalf("a fresh directory fails its own sum: %v", err)
	}

	f := globSorted(t, filepath.Join(out, "*.up.sql"))[0]
	b, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(b, []byte("-- edited after the fact\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"verify", "-sum", "-out", out},
		{"verify", "-pending", "-dsn", dsn(t), "-out", out},
		{"diff", "next", "-dsn", dsn(t), "-schema", ns, "-out", out},
	} {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "modified") {
			t.Errorf("%v accepted an edited migration: %v", args, err)
		}
	}
	if err := run([]string{"sum", "-out", out}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-sum", "-out", out}); err != nil {
		t.Fatalf("after storm sum: %v", err)
	}
}

func TestCLI_DiffDryRunWritesNothing(t *testing.T) {
	ns := namespace(t, "cli_dry")
	out := filepath.Join(t.TempDir(), "migrations")
	withModels(t, []any{&noteV1{}})
	if err := run([]string{"diff", "init", "-dry-run", "-down", "-dsn", dsn(t), "-schema", ns, "-out", out}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("-dry-run created %s (%v)", out, err)
	}
}

// verify -sum and sum need neither a database nor a model: they are the
// first step of a CI job and run before either exists.
func TestCLI_SumNeedsNoModelAndNoDatabase(t *testing.T) {
	withModels(t, nil)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "0001_x.up.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-sum", "-out", out}); err == nil || !strings.Contains(err.Error(), "storm sum") {
		t.Fatalf("an unsummed directory should point at storm sum, got %v", err)
	}
	if err := run([]string{"sum", "-out", out, "-dialect", "mysql"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-sum", "-out", out, "-dialect", "mysql"}); err != nil {
		t.Fatal(err)
	}
}
