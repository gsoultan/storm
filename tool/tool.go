// Package tool is the developer-facing command set — render DDL, diff a
// migration, generate packages, and verify that model, generated code,
// migrations and the live database all still agree — as a LIBRARY.
//
// It is a library because the commands need your models, and a binary
// installed from this repository cannot see them. So the binary in cmd/raorm
// is a stub, and the real tool is five lines in your own module:
//
//	package main
//
//	import (
//		"github.com/gsoultan/raorm/tool"
//		"example.com/app/model"
//	)
//
//	func main() { tool.Main(model.All(), model.Queries()) }
//
// Then `go run ./cmd/raorm generate`, `... verify -pending`, `... lint` and
// the rest work against YOUR schema, and `go tool` can install it into your
// module if you want the short form.
//
// raorm never applies DDL. Every command either prints SQL or exits non-zero.
package tool

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

// hostModule reads the module path of the module being generated INTO — the
// caller's, not raorm's.
//
// Generated packages import each other (the context package imports one
// package per table), so those imports have to spell the host module's path.
// This used to be raorm's own constant, which meant `generate` emitted
// `github.com/gsoultan/raorm/internal/store/user` into somebody else's
// module and produced code that could not compile. It went unnoticed because
// every generation that ever ran was inside this repository, where the wrong
// answer is the right one — which is exactly the bug a first outside user
// finds in their first five minutes.
func hostModule(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if rest == "" || rest == "(" {
			continue // `module (` blocks do not exist, but do not guess
		}
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		return strings.Trim(rest, "\"`"), nil
	}
	return "", fmt.Errorf("no module directive in %s/go.mod — generated packages import each other and need the module path", root)
}

// Models is the model set the commands operate on. Main sets it; it stays a
// package variable rather than a parameter threaded through twenty functions
// because every command needs it and none of them may choose a different one.
var Models []any

// RawQueries is every raorm.SQL[T] and raorm.SQLExec declaration that wants a
// generated scanner and build-time validation.
var RawQueries []raorm.RawDecl

const usage = `raorm — a compile-time ORM for PostgreSQL

usage:
  raorm ddl                       print CREATE statements for the model
  raorm diff   <name>             write a migration from the live schema to the model
  raorm verify                    fail if the database has drifted from the model
  raorm verify -stale [dir]       fail if generated code is stale (no database needed)
  raorm verify -pending           fail if the model has changes no migration carries
  raorm import                    print the model implied by an existing database
  raorm generate [dir]            emit one Go package per table (default internal/store)
                                  -raw-schema live validates raw SQL against the
                                  connected database rather than a scratch apply
                                  of the model, for schemas whose truth is
                                  migrations (functions, views the model omits)
  raorm lint                      cost every named plan in round trips; fail over the budget
  raorm explain                   plan every statement; flag large seq scans (PostgreSQL 16+)

flags:
  -dsn        PostgreSQL connection string (or $RAORM_DSN)
  -schema     namespace to read/write (default "public")
  -out        migrations directory (default "db/migrations")
  -allow-destructive
              permit steps that can lose data; without it, diff refuses
`

// Main is the whole tool: register your models and hand it the command line.
// It exits the process, because a developer tool that returned an error for
// its caller to print would have two error formats.
func Main(models []any, queries []raorm.RawDecl) {
	Models, RawQueries = models, queries
	if err := Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "raorm: "+err.Error())
		os.Exit(1)
	}
}

// Run executes one command against the registered models. Exported for tests
// and for a caller that wants to embed the tool in a larger command.
func Run(args []string) error { return run(args) }

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
	rawSchema := fs.String("raw-schema", string(RawAgainstModel),
		"validate raw SQL declarations against \"model\" (a scratch apply) or \"live\" (the connected database)")
	maxTrips := fs.Int("max-round-trips", 4, "lint: the most round trips a named plan may cost")
	maxSeqRows := fs.Int("max-seq-rows", 10000, "explain: flag a seq scan the planner sizes at or above this")
	pending := fs.Bool("pending", false, "verify the model against the migrations directory instead of the database")
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

	case "explain":
		return explain(*dsn, *ns, model, *maxSeqRows)

	case "lint":
		return lint(model, *maxTrips)

	case "generate":
		dir := "internal/store"
		if nargs > 0 {
			dir = arg(0)
		}
		against, err := parseRawSchema(*rawSchema)
		if err != nil {
			return err
		}
		return generate(dir, model, *dsn, against)

	case "verify":
		if *pending {
			return verifyPending(*dsn, *out, model)
		}
		if *stale {
			dir := "internal/store"
			if nargs > 0 {
				dir = arg(0)
			}
			against, err := parseRawSchema(*rawSchema)
			if err != nil {
				return err
			}
			return verifyStale(dir, model, against)
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

// moduleRoot walks up from the working directory to the go.mod.
// resolveOutDir pins an output directory to the module: the absolute path to
// write to, and the module-relative path the IMPORT is built from. One
// function because generate and verify -stale must agree byte-for-byte — the
// first split between them reported freshly-generated code as stale.
func resolveOutDir(dir string) (absDir, rel, hostMod string, err error) {
	root, err := moduleRoot()
	if err != nil {
		return "", "", "", err
	}
	hostMod, err = hostModule(root)
	if err != nil {
		return "", "", "", err
	}
	abs := dir
	if !filepath.IsAbs(abs) {
		if abs, err = filepath.Abs(dir); err != nil {
			return "", "", "", err
		}
	}
	// Resolve symlinks on both sides before comparing. On macOS /tmp is a
	// symlink to /private/tmp, so an output directory under it looks "outside
	// the module" to a plain string comparison — a confusing refusal for a
	// path that is plainly inside.
	abs, root = resolveSymlinks(abs), resolveSymlinks(root)
	rel, err = filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", "", fmt.Errorf(
			"the output directory %s is outside this module — generated code's import "+
				"path cannot point outside it; use a directory under %s", dir, root)
	}
	return filepath.Join(root, rel), rel, hostMod, nil
}

// resolveSymlinks resolves the deepest EXISTING ancestor of a path and
// re-appends the rest.
//
// Plain EvalSymlinks fails on a path that does not exist, which an output
// directory usually does not yet — and resolving only one side of the
// comparison is worse than resolving neither, because then the two sides
// disagree about /tmp vs /private/tmp.
func resolveSymlinks(path string) string {
	var trailing []string
	cur := path
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(trailing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, trailing[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path // nothing along the way exists; leave it alone
		}
		trailing = append(trailing, filepath.Base(cur))
		cur = parent
	}
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory — run raorm inside the module")
		}
		dir = parent
	}
}

// prepareRawQueries PREPAREs every registered raorm.SQL declaration against
// the MODEL and resolves its scanner.
//
// Against the model, not the live schema: the model DDL is applied to a
// scratch namespace first, so a drifted dev database cannot vouch for a query
// the model would reject — and no pre-existing schema is needed at all, only a
// PostgreSQL server, which is what makes this workable in CI.
// RawSchema selects what a raw declaration is PREPAREd against.
type RawSchema string

const (
	// RawAgainstModel applies the model to a scratch schema and validates
	// there, so the MODEL vouches for every statement and a drifted
	// development database cannot make a broken query look fine. The default,
	// and the right answer whenever the model describes everything a query
	// touches.
	RawAgainstModel RawSchema = "model"

	// RawAgainstLive validates against the connected database as it is.
	//
	// It exists because the first adopter needed it and could not use the
	// tool without it: anubis's queries call SQL functions (`authorize()`)
	// and read tables its raorm model deliberately does not describe — the
	// model there is a PROJECTION of a schema whose source of truth is
	// migrations. Under RawAgainstModel every one of those statements fails
	// to prepare, which is correct and useless.
	//
	// The cost is stated rather than hidden: the model no longer vouches for
	// these statements, so a development database that has drifted from
	// migrations will happily validate a query production cannot run. Point
	// it at a database built by applying migrations from scratch — which is
	// what CI has anyway.
	RawAgainstLive RawSchema = "live"
)

func prepareRawQueries(dsn string, model *schema.Schema, against RawSchema) ([]codegen.RawScanner, error) {
	if len(RawQueries) == 0 {
		return nil, nil
	}
	if dsn == "" {
		return nil, errors.New(
			"raw queries are registered, and validating them needs -dsn (or $RAORM_DSN): " +
				"each statement is PREPAREd against a real server — a server is " +
				"required, an existing schema is not (unless -raw-schema live)")
	}
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return nil, err
	}
	defer done()

	if against == RawAgainstModel {
		scratch := fmt.Sprintf("raorm_sqlcheck_%d", os.Getpid())
		if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE; CREATE SCHEMA "+scratch); err != nil {
			return nil, err
		}
		defer func() { _, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE") }()
		if _, err := c.Exec(ctx, "SET search_path TO "+scratch); err != nil {
			return nil, err
		}
		if _, err := c.Exec(ctx, pgddl.Create(model)); err != nil {
			return nil, fmt.Errorf("apply model DDL to scratch schema: %w", err)
		}
	}

	var out []codegen.RawScanner
	for i, d := range RawQueries {
		rt, sql := raorm.DeclOf(d)
		name := "raorm.SQLExec"
		if rt != nil {
			name = "raorm.SQL[" + rt.Name() + "]"
		}
		sd, err := c.Prepare(ctx, fmt.Sprintf("raorm_sqlcheck_%d", i), sql)
		if err != nil {
			return nil, fmt.Errorf("%s does not prepare against the %s schema:\n  %w", name, against, err)
		}
		if rt == nil {
			// SQLExec: validated like any declaration, but it must not
			// return rows — an exec that reads as "fire and forget" while
			// the server sends a result set is a bug, not a convenience.
			if len(sd.Fields) > 0 {
				return nil, fmt.Errorf(
					"%s returns %d column(s) — use raorm.SQL[T] to read them, or make the statement return nothing",
					name, len(sd.Fields))
			}
			continue
		}
		fields := make([]codegen.RawField, len(sd.Fields))
		for j, f := range sd.Fields {
			fields[j] = codegen.RawField{Name: string(f.Name), OID: f.DataTypeOID}
		}
		rs, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), fields)
		if err != nil {
			return nil, fmt.Errorf("%s\n  %w", name, err)
		}
		out = append(out, rs)
	}
	return out, nil
}

// lint costs every named plan and fails when one exceeds the budget.
//
// The cost is knowable at generate time — one round trip for the parents, one
// per relation, one per nested relation — which is the point of plans being a
// declared, reviewable artifact rather than call sites: the one file listing
// every load pattern in the system can be BUDGETED, and a plan that quietly
// grew past the budget fails CI naming itself, instead of failing a latency
// SLO in production naming nothing.
func lint(model *schema.Schema, maxTrips int) error {
	var names []string
	for _, t := range model.Tables {
		names = append(names, t.Name)
	}
	costs, err := codegen.PlanCosts(model, names)
	if err != nil {
		return err
	}
	if len(costs) == 0 {
		fmt.Println("no named plans declared — nothing to lint")
		return nil
	}

	over := 0
	for _, c := range costs {
		mark := "✓"
		if c.RoundTrips > maxTrips {
			mark = "✗"
			over++
		}
		fmt.Printf("  %s %-16s %d round trip(s)   %s\n", mark, c.Name, c.RoundTrips, c.Chain)
	}
	if over > 0 {
		return fmt.Errorf("%d plan(s) exceed the budget of %d round trips — split the plan, "+
			"or raise -max-round-trips if the cost is intended", over, maxTrips)
	}
	fmt.Printf("✓ no plan exceeds %d round trips\n", maxTrips)
	return nil
}

// generate emits one package per table under dir.
//
// Every file is rendered before any is written. A generation that fails on the
// ninth table must not leave eight new packages and a broken build behind —
// `raorm verify` would then be comparing against a tree nobody intended.
// parseRawSchema keeps a typo from silently selecting the wrong validation.
func parseRawSchema(v string) (RawSchema, error) {
	switch RawSchema(v) {
	case RawAgainstModel:
		return RawAgainstModel, nil
	case RawAgainstLive:
		return RawAgainstLive, nil
	}
	return "", fmt.Errorf("-raw-schema %q: want %q or %q", v, RawAgainstModel, RawAgainstLive)
}

func generate(dir string, model *schema.Schema, dsn string, against RawSchema) error {
	// The import path is derived from the directory, which only means anything
	// module-relative: gluing an ABSOLUTE path onto the module path produces
	// an import that cannot compile — found the moment a test finally BUILT
	// the output instead of reading it. An absolute dir is made relative to
	// the working directory (the module root, where raorm runs), and one that
	// escapes the module is an error naming why, not a tree that fails later.
	dir, rel, hostMod, err := resolveOutDir(dir)
	if err != nil {
		return err
	}
	scanners, err := prepareRawQueries(dsn, model, against)
	if err != nil {
		return err
	}
	files, err := codegen.Package(model, codegen.PackageOptions{
		Dir:           dir,
		Import:        modulePath,
		Package:       filepath.Base(dir),
		PackageImport: hostMod + "/" + filepath.ToSlash(rel),
		RawScanners:   scanners,
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
func verifyStale(dir string, model *schema.Schema, against RawSchema) error {
	dir, rel, hostMod, err := resolveOutDir(dir)
	if err != nil {
		return err
	}
	scanners, err := prepareRawQueries(os.Getenv("RAORM_DSN"), model, against)
	if err != nil {
		return err
	}
	want, err := codegen.Package(model, codegen.PackageOptions{
		Dir:           dir,
		Import:        modulePath,
		Package:       filepath.Base(dir),
		PackageImport: hostMod + "/" + filepath.ToSlash(rel),
		RawScanners:   scanners,
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

// verifyPending is ADR-0001's third mode: model against MIGRATIONS. "Changed
// the model, forgot to run diff" becomes a CI failure instead of a deploy that
// quietly runs against a schema the code no longer describes.
//
// Mechanism: replay every migration into a scratch namespace, then diff the
// result against the model. A non-empty diff is exactly the SQL the missing
// migration would contain, so the failure prints its own fix. It needs a
// database (the same dev database ADR-0001 already assumes) but never touches
// a real namespace — the scratch schema is dropped on every exit path.
func verifyPending(dsn, out string, model *schema.Schema) error {
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return err
	}
	defer done()

	scratch := fmt.Sprintf("raorm_pending_%d", os.Getpid())
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE; CREATE SCHEMA "+scratch); err != nil {
		return fmt.Errorf("create scratch schema: %w", err)
	}
	defer func() { _, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE") }()

	files, err := filepath.Glob(filepath.Join(out, "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files) // the numbered prefix is the replay order
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := c.Exec(ctx, "SET search_path TO "+scratch+"; "+string(sql)); err != nil {
			return fmt.Errorf("replaying %s: %w", filepath.Base(f), err)
		}
	}

	plan, err := migrate.For(ctx, c, scratch, model)
	if err != nil {
		return err
	}
	if plan.Empty() {
		fmt.Printf("✓ %d migration(s) carry every model change\n", len(files))
		return nil
	}
	fmt.Fprintf(os.Stderr, "the model has %d change(s) no migration carries:\n\n%s\n",
		len(plan.Changes), plan.SQL())
	return fmt.Errorf("model changed without a migration — run 'raorm diff <name>' and commit the result")
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
