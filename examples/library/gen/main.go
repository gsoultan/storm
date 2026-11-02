// Command gen regenerates the MongoDB example's store and collection
// definitions from its model.
//
//	go run ./examples/library/gen
//
// A real module runs `storm generate -dialect mongo` and `storm ddl -dialect
// mongo`; this is the same pair of calls, kept local so the example
// regenerates in CI without an installed binary.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen/mongo"
	"github.com/gsoultan/storm/compile/mgddl"
	"github.com/gsoultan/storm/examples/library/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	s, err := storm.Build(model.All()...)
	if err != nil {
		return err
	}
	dir := filepath.Join("examples", "library", "store")
	files, err := codegenmongo.Package(s, codegenmongo.Options{Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "github.com/gsoultan/storm/examples/library/store"})
	if err != nil {
		return err
	}
	ddl, err := mgddl.Create(s)
	if err != nil {
		return err
	}
	files["collections.js"] = []byte(ddl)
	var paths []string
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
