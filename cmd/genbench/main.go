package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/codegen"
	"github.com/gsoultan/raorm/internal/benchmodel"
)

func main() {
	s, err := raorm.Build(benchmodel.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	src, err := codegen.File(s, codegen.Options{
		Package: "genuser",
		Import:  "github.com/gsoultan/raorm",
		Table:   "users",
		// Match the spike's statement exactly, so the comparison is of the
		// generator and not of two different query plans.
		OrderBy: `"created_at" DESC, "id"`,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if len(src) > 0 {
			os.Stderr.Write(src)
		}
		os.Exit(1)
	}
	out := filepath.Join("bench", "genuser")
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f := filepath.Join(out, "user.gen.go")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("→ %s (%d bytes)\n", f, len(src))
}
