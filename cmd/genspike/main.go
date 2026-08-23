// Command genspike generates the two table packages the M3 plan-type spike
// sits on top of.
//
// The table packages are *generated*; the plan layer above them in
// internal/planspike is *hand-written*. That is the same split as the M0 spike,
// for the same reason: "is the design usable?" and "can a generator emit it?"
// are two failures that are otherwise indistinguishable, and the second is only
// worth four weeks once the first is answered.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/codegen"
	"github.com/gsoultan/raorm/internal/testmodel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	s, err := raorm.Build(testmodel.All()...)
	if err != nil {
		return err
	}
	dir := filepath.Join("internal", "planspike", "store")
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           dir,
		Import:        "github.com/gsoultan/raorm",
		Only:          []string{"orgs", "users"},
		Package:       "store",
		PackageImport: "github.com/gsoultan/raorm/internal/planspike/store",
	})
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
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
	return nil
}
