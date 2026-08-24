// Command raorm is the developer-facing tool: render DDL, diff a migration,
// and verify that model, generated code, migrations and the live database all
// still agree.
//
// raorm never applies DDL. Every command either prints SQL or exits non-zero.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/codegen"
	"github.com/gsoultan/raorm/compile/pgddl"
	"github.com/gsoultan/raorm/migrate"
	"github.com/gsoultan/raorm/schema"
	pgintro "github.com/gsoultan/raorm/schema/pg"
	"github.com/jackc/pgx/v5"
)

// modulePath is raorm's own import path, which generated code imports for the
// runtime. It is a constant because a generated package that pointed at a fork
// or a stale vendor copy would compile and be subtly wrong.
const modulePath = "github.com/gsoultan/raorm"

// Models is set by the generated bootstrap in the user's module. Keeping it a
// variable rather than a plugin keeps the tool a plain Go binary.
var Models []any

const usage = `raorm — a compile-time ORM for PostgreSQL

usage:
  raorm ddl                       print CREATE statements for the model
  raorm diff   <name>             write a migration from the live schema to the model
  raorm verify                    fail if the database has drifted from the model
  raorm verify -stale [dir]       fail if generated code is stale (no database needed)
  raorm import                    print the model implied by an existing database
  raorm generate [dir]            emit one Go package per table (default internal/store)

flags:
  -dsn        PostgreSQL connection string (or $RAORM_DSN)
  -schema     namespace to read/write (default "public")
  -out        migrations directory (default "db/migrations")
  -allow-destructive
              permit steps that can lose data; without it, diff refuses
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "raorm: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	// The command comes first and the flags follow it, which is what everyone
	// expects and what `flag` does not do on its own: Parse stops at the first
	// non-flag argument, so parsing the whole slice leaves every flag after the
	// subcommand unread. `raorm verify -stale` silently became `raorm verify`
	// and asked for a database.
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}
	cmd, args := args[0], args[1:]

	fs := flag.NewFlagSet("raorm "+cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dsn := fs.String("dsn", os.Getenv("RAORM_DSN"), "PostgreSQL connection string")
	ns := fs.String("schema", "public", "namespace")
	out := fs.String("out", "db/migrations", "migrations directory")
	allowDestructive := fs.Bool("allow-destructive", false, "permit data-losing steps")
	stale := fs.Bool("stale", false, "verify generated code against the model instead of the database")
	// Flags may appear ANYWHERE, including after a positional argument.
	//
	// Go's flag package stops at the first non-flag, so `raorm diff init
	// -schema mine` parses no flags at all and -schema silently keeps its
	// default of "public". For -allow-destructive that fails safe; for -schema
	// it means diffing the wrong namespace and proposing to drop objects that
	// belong to it. That is a data-availability incident caused by argument
	// order, which is not a trade worth making to save this loop.
	var positional []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	arg := func(i int) string {
		if i < len(positional) {
			return positional[i]
		}
		return ""
	}
	nargs := len(positional)
	model, err := buildModel()
	if err != nil {
		return err
	}

	switch cmd {
	case "ddl":
		fmt.Print(pgddl.Create(model))
		return nil

	case "diff":
		if nargs < 1 {
			return errors.New("diff needs a name: raorm diff add_user_status")
		}
		return diff(*dsn, *ns, *out, arg(0), model, *allowDestructive)

	case "generate":
		dir := "internal/store"
		if nargs > 0 {
			dir = arg(0)
		}
		return generate(dir, model)

	case "verify":
		if *stale {
			dir := "internal/store"
			if nargs > 0 {
				dir = arg(0)
			}
			return verifyStale(dir, model)
		}
		return verify(*dsn, *ns, model)

	case "import":
		return importSchema(*dsn, *ns)

	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func buildModel() (*schema.Schema, error) {
	if len(Models) == 0 {
		return nil, errors.New(
			"no models registered — this binary is a template; generate one for your module\n" +
				"       (see docs/EXAMPLE.md §2)")
	}
	return raorm.Build(Models...)
}

func connect(dsn string) (*pgx.Conn, context.Context, func(), error) {
	if dsn == "" {
		return nil, nil, nil, errors.New("no -dsn and no $RAORM_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return c, ctx, func() { c.Close(ctx); cancel() }, nil
}

// generate emits one package per table under dir.
//
// Every file is rendered before any is written. A generation that fails on the
// ninth table must not leave eight new packages and a broken build behind —
// `raorm verify` would then be comparing against a tree nobody intended.
func generate(dir string, model *schema.Schema) error {
	files, err := codegen.Package(model, codegen.PackageOptions{
		Dir:           dir,
		Import:        modulePath,
		Package:       filepath.Base(dir),
		PackageImport: modulePath + "/" + filepath.ToSlash(dir),
	})
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths) // stable output order, so a diff of two runs is empty

	for _, rel := range paths {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, files[rel], 0o644); err != nil {
			return err
		}
		fmt.Printf("→ %s (%d bytes)\n", full, len(files[rel]))
	}
	fmt.Printf("%d package(s) from %d table(s)\n", len(files), len(model.Tables))
	return nil
}

func diff(dsn, ns, out, name string, model *schema.Schema, allowDestructive bool) error {
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return err
	}
	defer done()

	plan, err := migrate.For(ctx, c, ns, model)
	if err != nil {
		return err
	}
	if plan.Empty() {
		fmt.Println("no changes — the database already matches the model")
		return nil
	}
	if plan.Destructive() && !allowDestructive {
		fmt.Fprint(os.Stderr, plan.SQL())
		return errors.New("plan contains steps that can lose data; re-run with -allow-destructive " +
			"once you have read them")
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	seq, err := nextSeq(out)
	if err != nil {
		return err
	}
	file := filepath.Join(out, fmt.Sprintf("%04d_%s.up.sql", seq, name))
	header := fmt.Sprintf("-- generated by raorm; review before applying\n-- %d step(s)\n\n", len(plan.Changes))
	if err := os.WriteFile(file, []byte(header+plan.SQL()), 0o644); err != nil {
		return err
	}
	fmt.Printf("→ %s (%d steps%s)\n", file, len(plan.Changes),
		map[bool]string{true: ", DESTRUCTIVE"}[plan.Destructive()])
	fmt.Println("review and commit — raorm never applies a migration")
	return nil
}

// verifyStale reports whether the generated code on disk is what the model
// would produce now.
//
// This is the check that belongs in CI, and it is the one that needs no
// database: a model change without a regenerate leaves code that compiles, runs
// and queries the wrong columns. Drift against the database is a different
// question, answered by verify without -stale.
//
// It compares bytes rather than regenerating in place, so a CI run cannot
// "fix" the problem by rewriting the tree it was asked to check.
func verifyStale(dir string, model *schema.Schema) error {
	want, err := codegen.Package(model, codegen.PackageOptions{
		Dir:           dir,
		Import:        modulePath,
		Package:       filepath.Base(dir),
		PackageImport: modulePath + "/" + filepath.ToSlash(dir),
	})
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var stale, missing []string
	for _, rel := range paths {
		full := filepath.Join(dir, rel)
		got, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, full)
			continue
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want[rel]) {
			stale = append(stale, full)
		}
	}

	if len(stale) == 0 && len(missing) == 0 {
		fmt.Printf("✓ generated code matches the model (%d file(s))\n", len(want))
		return nil
	}
	for _, f := range missing {
		fmt.Fprintf(os.Stderr, "missing: %s\n", f)
	}
	for _, f := range stale {
		fmt.Fprintf(os.Stderr, "stale:   %s\n", f)
	}
	return fmt.Errorf("generated code is out of date — run 'raorm generate %s'", dir)
}

func verify(dsn, ns string, model *schema.Schema) error {
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return err
	}
	defer done()

	plan, err := migrate.For(ctx, c, ns, model)
	if err != nil {
		return err
	}
	if plan.Empty() {
		fmt.Println("✓ database matches the model")
		return nil
	}
	fmt.Fprintf(os.Stderr, "database has drifted from the model — %d pending change(s):\n\n%s",
		len(plan.Changes), plan.SQL())
	return errors.New("drift detected")
}

func importSchema(dsn, ns string) error {
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return err
	}
	defer done()
	s, err := pgintro.Introspect(ctx, c, ns)
	if err != nil {
		return err
	}
	// A Go MODEL, not the DDL. raorm is model-first, so adopting an existing
	// database means having a model to start from — and hand-writing one for
	// forty tables is where adoption stops. The DDL is already in the database;
	// printing it back would help nobody.
	src, err := codegen.Model(s, codegen.ModelOptions{Package: "model", Import: modulePath})
	if err != nil {
		return err
	}
	os.Stdout.Write(src)
	return nil
}

// nextSeq finds the next migration number, so two people generating on
// different branches collide in git rather than silently reordering.
func nextSeq(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	max := 0
	for _, e := range entries {
		n := 0
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &n); err == nil && n > max {
			max = n
		}
	}
	return max + 1, nil
}

var _ = strings.TrimSpace
