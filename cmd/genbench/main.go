package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/internal/benchmodel"
)

func main() {
	s, err := storm.Build(benchmodel.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	src, err := codegen.File(s, codegen.Options{
		Package: "genuser",
		Import:  "github.com/gsoultan/storm",
		Table:   "users",
		// Match the spike's statement exactly, so the comparison is of the
		// generator and not of two different query plans.
		// The bench table is loaded by org_id, so it needs the per-parent
		// loader that the strategy benchmark compares.
		BatchTopColumns: []string{"org_id"},
		DefaultOrder: []codegen.OrderTerm{
			{Column: "created_at", Desc: true},
			{Column: "id"},
		},
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
