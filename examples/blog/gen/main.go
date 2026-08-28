// Command gen regenerates the example's store from its model.
//
//	go run ./examples/blog/gen
//
// A real module just runs `storm generate` — the installed binary discovers the
// models and writes its own bootstrap (ADR-0006). This small main is the same
// call that bootstrap makes, kept local so the example generates itself in CI
// without depending on an installed binary.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/examples/blog/model"
)

func main() {
	s, err := storm.Build(model.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := filepath.Join("examples", "blog", "store")
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           dir,
		Import:        "github.com/gsoultan/storm",
		Package:       "store",
		PackageImport: "github.com/gsoultan/storm/examples/blog/store",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(full, files[rel], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("→ %s (%d bytes)\n", full, len(files[rel]))
	}
}
