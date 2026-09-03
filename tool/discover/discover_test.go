package tooldiscover

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Discovery is an inference about somebody else's source, and every rule in it
// is a guess that has to be right on code storm has never seen. The fixtures
// under testdata are the shapes that make the guesses hard: a mixin that looks
// exactly like a model, a model that embeds nothing, an aliased import, and a
// generated file that would otherwise be discovered as input to the run that
// wrote it.

func discover(t *testing.T, fixture string) *Result {
	t.Helper()
	r, err := Discover(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("Discover(%s): %v", fixture, err)
	}
	return r
}

func names(r *Result) []string {
	out := make([]string, 0, len(r.Models))
	for _, m := range r.Models {
		out = append(out, m.TypeName)
	}
	return out
}

func TestDiscoverFindsModelsAndNothingElse(t *testing.T) {
	r := discover(t, "basic")

	// WithLocal is deliberately absent: it embeds a mixin but carries no storm
	// marker of its own, so it is an ordinary struct. Discovery infers, and
	// inferring a table from "embeds something that has a Schema method" would
	// make every struct in a module a candidate.
	want := []string{"Invoice", "Member", "Region", "Tagged", "Team"}
	if got := names(r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", got, want)
	}
}

func TestDiscoverReasons(t *testing.T) {
	r := discover(t, "basic")
	want := map[string]Reason{
		"Team":   EmbedsModel, // embed outranks its Schema method
		"Member": EmbedsModel,
		"Region": HasSchema, // a natural key: embeds nothing at all
		"Tagged": Directive, // no storm marker except the directive
	}
	got := map[string]Reason{}
	for _, m := range r.Models {
		got[m.TypeName] = m.Why
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: reason = %q, want %q", name, got[name], w)
		}
	}
}

// A mixin matches every rule a model matches. Being embedded is the only thing
// that separates them, and mixins are routinely exported — internal/testmodel
// has two — so this is the failure that would generate a table per mixin.
func TestMixinsAreNotModels(t *testing.T) {
	r := discover(t, "basic")
	for _, m := range r.Models {
		switch m.TypeName {
		case "Base":
			t.Error("cross-package mixin shared.Base was reported as a model")
		case "Local":
			t.Error("same-package mixin Local was reported as a model")
		}
	}
	var sawLocal bool
	for _, s := range r.Skipped {
		if s.Name == "Local" && strings.Contains(s.Why, "mixin") {
			sawLocal = true
		}
	}
	if !sawLocal {
		t.Error("Local was dropped without being reported as a mixin")
	}
}

// The evidence that Local is a mixin lives in a file that imports nothing from
// storm. Skipping such files for speed would silently turn a mixin into a table.
func TestEmbedEvidenceFromAStormFreeFile(t *testing.T) {
	r := discover(t, "basic")
	for _, m := range r.Models {
		if m.TypeName == "Local" {
			t.Fatal("Local is embedded only in embeds.go, which imports no storm type")
		}
	}
}

func TestIgnoreDirective(t *testing.T) {
	r := discover(t, "basic")
	for _, m := range r.Models {
		if m.TypeName == "Legacy" {
			t.Error("//storm:ignore did not exclude Legacy")
		}
	}
}

// An unexported MIXIN is correct code. Warning about it on every command is
// how a warning becomes noise, so only unreachable-and-not-embedded types are
// marked actionable.
func TestUnexportedMixinIsNotActionable(t *testing.T) {
	r := discover(t, "basic")
	for _, s := range r.Skipped {
		if s.Name == "hidden" && s.Actionable {
			t.Error("an unexported mixin was flagged as actionable; it is correct code")
		}
	}
	var sawActionable bool
	for _, s := range r.Skipped {
		if s.Name == "notAModel" && s.Actionable {
			sawActionable = true
		}
	}
	if !sawActionable {
		t.Error("an unexported, un-embedded model candidate was not flagged; that one IS the mistake")
	}
}

func TestUnexportedIsSkippedNotFailed(t *testing.T) {
	r := discover(t, "basic")
	for _, m := range r.Models {
		if m.TypeName == "notAModel" {
			t.Error("an unexported type was reported as a model; no bootstrap can reference it")
		}
	}
	var found bool
	for _, s := range r.Skipped {
		if s.Name == "notAModel" {
			found = true
		}
	}
	if !found {
		t.Error("the unexported type was dropped silently")
	}
}

// Generated code embeds storm.Model too. Discovering it would make the second
// `generate` see tables the first one wrote, which is a loop that never
// converges.
func TestGeneratedFilesAreNotInput(t *testing.T) {
	r := discover(t, "basic")
	for _, m := range r.Models {
		if m.TypeName == "Ghost" {
			t.Error("a `Code generated ... DO NOT EDIT.` file was scanned as model source")
		}
	}
}

func TestRawQueries(t *testing.T) {
	r := discover(t, "basic")
	var got []string
	for _, q := range r.Queries {
		got = append(got, q.VarName)
	}
	want := "Declared,Purge,Top"
	if strings.Join(got, ",") != want {
		t.Errorf("queries = %v, want %s", got, want)
	}
	for _, q := range r.Queries {
		if q.VarName == "unexported" {
			t.Error("an unexported raw query was registered; it cannot be referenced")
		}
	}
}

// A package main with no models is ordinary. Only one that HAS models is an
// error, because nothing can import it to register them.
func TestPackageMainWithoutModelsIsFine(t *testing.T) {
	discover(t, "basic")
}

func TestModelInPackageMainIsNamed(t *testing.T) {
	_, err := Discover(filepath.Join("testdata", "inmain"))
	if err == nil {
		t.Fatal("a model in package main was accepted; nothing can import it")
	}
	for _, want := range []string{"package main", "Thing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// Go's internal rule is not advisory: a bootstrap at the module root cannot
// import a/internal/model, and the resulting error appears in a file the
// developer never wrote.
func TestInternalCeilingIsReported(t *testing.T) {
	r := discover(t, "split")
	if _, err := r.ShimDir(); err == nil {
		t.Fatal("ShimDir accepted a root that cannot import a/internal/model")
	} else if !strings.Contains(err.Error(), "internal") {
		t.Errorf("error does not explain the internal rule: %v", err)
	}
}

func TestShimDirIsDeepestCommonAncestor(t *testing.T) {
	r := discover(t, "basic")
	dir, err := r.ShimDir()
	if err != nil {
		t.Fatal(err)
	}
	// billing/ and model/ both hold models, so the shim must sit above both —
	// not in either one.
	want, err := filepath.Abs(filepath.Join("testdata", "basic"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Errorf("ShimDir = %s, want %s", dir, want)
	}
}

func TestOrderIsDeterministic(t *testing.T) {
	// Generated output must be byte-identical across runs, and the model order
	// reaches it. Map iteration is random, so this fails loudly rather than
	// one run in twenty.
	first := names(discover(t, "basic"))
	for i := 0; i < 20; i++ {
		if got := names(discover(t, "basic")); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d: %v != %v", i, got, first)
		}
	}
}

// A storm.SQL call that is not a package-level var is reported, because it can
// never be registered and storm refuses to run an unregistered statement. The
// error belongs at generate time, not at the first request down that branch.
func TestDiscoverReportsUndeclarableQueries(t *testing.T) {
	r, err := Discover(filepath.Join("testdata", "basic"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Undeclarable) != 3 {
		t.Fatalf("got %d undeclarable declaration(s), want 3: %+v", len(r.Undeclarable), r.Undeclarable)
	}
	var fns []string
	for _, u := range r.Undeclarable {
		fns = append(fns, u.Fn)
		if !strings.Contains(u.Pos, "dynamic.go") {
			t.Errorf("position %q does not name the file", u.Pos)
		}
		if u.Why == "" {
			t.Errorf("%s at %s was reported with no reason", u.Fn, u.Pos)
		}
	}
	sort.Strings(fns)
	// Both halves of the escape hatch, and the registry call that would
	// whitelist a statement built at run time.
	want := []string{"storm.RegisterStatement", "storm.SQL", "storm.SQLExec"}
	for i := range want {
		if fns[i] != want[i] {
			t.Fatalf("reported %v, want %v", fns, want)
		}
	}

	// The package-level declarations in the same package are untouched: the
	// rule is about where a declaration lives, not that raw SQL is suspect.
	var top bool
	for _, q := range r.Queries {
		if q.VarName == "Top" {
			top = true
		}
	}
	if !top {
		t.Error("a legitimate package-level declaration stopped being discovered")
	}
}
