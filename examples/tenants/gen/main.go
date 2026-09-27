// Command gen regenerates the sharded example's store from its model.
//
//	go run ./examples/tenants/gen
//
// Same shape as examples/blog/gen: a real module runs `storm generate`, and
// this local main is the call its bootstrap makes, kept here so the example
// regenerates in CI without an installed binary.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/examples/tenants/model"
)

func main() {
	s, err := storm.Build(model.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dir := filepath.Join("examples", "tenants", "store")
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           dir,
		Import:        "github.com/gsoultan/storm",
		Package:       "store",
		PackageImport: "github.com/gsoultan/storm/examples/tenants/store",
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
