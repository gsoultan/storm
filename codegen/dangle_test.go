package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// A reference the model declared AcknowledgeNoFK on can name a row that is not
// there: nothing stops the target from being deleted, or it lives in another
// database. A plan that loads it must say so with a nil, not fail the whole
// read with "references a missing row" — that error is right only where a
// foreign key makes the missing row impossible.

type nfUser struct {
	storm.Model
	Name string
}

func (u *nfUser) Schema(t *storm.Table) { t.Col(&u.Name).Size(40) }

type nfEvent struct {
	storm.Model
	Kind    string
	Owner   nfUser
	Subject nfUser
	Witness *nfUser
}

func (e *nfEvent) Schema(t *storm.Table) {
	t.Col(&e.Kind).Size(40)
	t.Col(&e.Subject).AcknowledgeNoFK("events outlive what they describe")
	t.Col(&e.Witness).AcknowledgeNoFK("events outlive what they describe")
}

func (e *nfEvent) Plans(p *storm.Plans) { p.Named("Story").With(&e.Owner).With(&e.Subject) }

func buildDangle(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&nfUser{}, &nfEvent{})
	if err != nil {
		t.Fatalf("a valid model did not build: %v", err)
	}
	return s
}

// planSource returns the text of one generated declaration and everything up
// to the next plan's row type.
func planSource(t *testing.T, src, rowType string) string {
	t.Helper()
	head := "type " + rowType + " struct {"
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("no %s was generated", rowType)
	}
	rest := src[i+len(head):]
	if j := strings.Index(rest, "Row struct {"); j >= 0 {
		if k := strings.LastIndex(rest[:j], "\ntype "); k >= 0 {
			rest = rest[:k]
		}
	}
	return head + rest
}

func TestAPlanOverAReferenceWithNoForeignKeyLoadsAMissingTargetAsNil(t *testing.T) {
	files, err := codegen.Package(buildDangle(t), codegen.PackageOptions{
		Dir: "store", Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "example.com/x/store",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["store.gen.go"])
	const missing = "references a missing"

	for _, c := range []struct {
		row, field string
		dangles    bool
	}{
		{"NfEventWithOwnerRow", "Owner nfuser.Row", false},
		{"NfEventWithSubjectRow", "Subject *nfuser.Row", true},
		{"NfEventWithWitnessRow", "Witness *nfuser.Row", true},
	} {
		plan := planSource(t, src, c.row)
		if !strings.Contains(plan, c.field) {
			t.Errorf("%s does not declare %q:\n%s", c.row, c.field, plan)
		}
		if got := strings.Contains(plan, missing); got == c.dangles {
			t.Errorf("%s: fails on a missing target = %v, want %v", c.row, got, !c.dangles)
		}
	}

	// The named plan loads both: the owner still refuses a missing row, the
	// subject does not.
	story := planSource(t, src, "NfEventStoryRow")
	if !strings.Contains(story, "Owner   nfuser.Row") || !strings.Contains(story, "Subject *nfuser.Row") {
		t.Errorf("the named plan's row is wrong:\n%s", story[:min(400, len(story))])
	}
	if n := strings.Count(story, missing); n != 1 {
		t.Errorf("the named plan fails on a missing target in %d places, want 1 (the owner)", n)
	}
}

// The behaviour, against a real database: an event whose subject was never
// there loads with a nil subject, and its neighbours load whole.
func TestAMissingTargetWithNoForeignKeyLoadsAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "nflive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(buildDangle(t), codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "github.com/gsoultan/storm/internal/" + base,
	})
	if err != nil {
		t.Fatal(err)
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
	src := strings.ReplaceAll(dangleLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a missing target does not load as nil:\n%s", out)
	}
	for _, name := range []string{
		"TestAPlanLoadsAMissingSubjectAsNil",
		"TestANamedPlanLoadsAMissingSubjectAsNil",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}
