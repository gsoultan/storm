// Command storm is the tool: install it once, run it in your module, write no
// bootstrap.
//
//	go install github.com/gsoultan/storm/cmd/storm@latest
//	storm generate internal/store
//
// The commands need your models, and a binary installed from this repository
// cannot see them — that much has always been true, and it is why this used to
// be a stub that asked you to write a five-line main. It only ever needed one
// static answer, though: which types are models and where do they live. So
// this finds them by parsing (tool/discover), writes the bootstrap itself,
// runs it, and removes it (tool/bootstrap).
//
// `storm watch <dir>` keeps the generated tree current as you edit, so the
// generate step stops being something to remember.
//
// A hand-written `tool.Main(model.All(), model.Queries())` keeps working
// exactly as before. Discovery is an addition, not a replacement — a module
// that has one is left alone.
//
// storm never applies DDL. Every command either prints SQL or exits non-zero.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	toolbootstrap "github.com/gsoultan/storm/tool/bootstrap"
	tooldiscover "github.com/gsoultan/storm/tool/discover"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "storm: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	// watch owns its own loop, and re-discovers on every pass so a NEW model
	// file is picked up rather than only edits to known ones.
	if len(args) > 0 && args[0] == "watch" {
		if len(args) < 2 {
			return errWatchNeedsDir
		}
		return watch(args[1], args[2:])
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	r, err := tooldiscover.Discover(wd)
	if err != nil {
		return err
	}

	if len(args) > 0 && args[0] == "models" {
		return printModels(r)
	}
	warn(r)

	if err := checkUndeclarable(r); err != nil {
		return err
	}
	code, err := toolbootstrap.Run(r, args)
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

// checkUndeclarable fails generation on a declaration the statement registry
// cannot vouch for: a storm.SQL call that is not a package-level var, or a
// RegisterStatement whose text is computed.
//
// A warning would be the wrong shape. storm refuses to run an unregistered
// statement — that is the check that keeps a caller's string out of SQL text —
// and a declaration built inside a function is never registered, so the code
// is already broken. Saying so here costs a build; saying nothing costs the
// first request that reaches the branch.
func checkUndeclarable(r *tooldiscover.Result) error {
	if len(r.Undeclarable) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d raw-SQL declaration(s) that defeat the statement registry:\n",
		len(r.Undeclarable))
	for _, u := range r.Undeclarable {
		fmt.Fprintf(&b, "  %s\n      %s here %s\n", u.Pos, u.Fn, u.Why)
	}
	b.WriteString("       a statement is declared once and its values are bound, never spelled:\n" +
		"         var Q = storm.SQL[Row](`SELECT ... WHERE tenant = $1`)\n" +
		"         rows, err := Q.Query(ctx, db, tenantID)")
	return errors.New(b.String())
}

// warn reports only what the developer probably did not mean. Mixins are
// skipped too, and are not mentioned: they are what correct code looks like,
// and a warning that fires on correct code is one people learn to scroll past.
// `storm models` shows everything.
func warn(r *tooldiscover.Result) {
	for _, s := range r.Skipped {
		if s.Actionable {
			fmt.Fprintf(os.Stderr, "storm: skipped %s.%s — %s\n\t%s\n", s.ImportPath, s.Name, s.Why, s.Pos)
		}
	}
}

// printModels backs `storm models`: every rule here is an inference about
// someone else's code, so there has to be a way to ask what it concluded that
// is cheaper than reading the generator's output and guessing backwards.
func printModels(r *tooldiscover.Result) error {
	fmt.Printf("module %s\n", r.Module.Path)
	if len(r.Models) == 0 {
		fmt.Println("\nno models found")
	} else {
		fmt.Printf("\n%d model(s):\n", len(r.Models))
		for _, m := range r.Models {
			fmt.Printf("  %s.%s\n      %s\n      %s\n", m.ImportPath, m.TypeName, m.Why, m.Pos)
		}
	}
	if len(r.Queries) > 0 {
		fmt.Printf("\n%d raw quer(ies):\n", len(r.Queries))
		for _, q := range r.Queries {
			fmt.Printf("  %s.%s\n      %s\n", q.ImportPath, q.VarName, q.Pos)
		}
	}
	if len(r.Skipped) > 0 {
		fmt.Printf("\n%d skipped:\n", len(r.Skipped))
		for _, s := range r.Skipped {
			fmt.Printf("  %s.%s\n      %s\n      %s\n", s.ImportPath, s.Name, s.Why, s.Pos)
		}
	}
	if dir, err := r.ShimDir(); err == nil && len(r.Models) > 0 {
		fmt.Printf("\nbootstrap would be written under %s\n", dir)
	}
	return nil
}
