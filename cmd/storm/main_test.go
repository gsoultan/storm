package main

import (
	"strings"
	"testing"

	tooldiscover "github.com/gsoultan/storm/tool/discover"
)

// The error a developer actually reads. It has to name the line, say what the
// rule found, and show the fix — a build failure that only says "refused"
// costs more time than the mistake did.
func TestCheckUndeclarableNamesLineAndFix(t *testing.T) {
	err := checkUndeclarable(&tooldiscover.Result{
		Undeclarable: []tooldiscover.Undeclarable{
			{Pos: "internal/store/reports.go:14", Fn: "storm.SQL", Why: "is not a package-level var"},
			{Pos: "internal/store/reports.go:31", Fn: "storm.RegisterStatement", Why: "registers a statement built at run time"},
		},
	})
	if err == nil {
		t.Fatal("two undeclarable declarations did not fail generation")
	}
	for _, want := range []string{
		"internal/store/reports.go:14",
		"internal/store/reports.go:31",
		"storm.SQL",
		"storm.RegisterStatement",
		"is not a package-level var",
		"registers a statement built at run time",
		"$1", // the fix, not just the complaint
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%s", want, err)
		}
	}
}

// Nothing found is not an error. A gate that fires on correct code is one
// people learn to route around.
func TestCheckUndeclarableSilentWhenClean(t *testing.T) {
	if err := checkUndeclarable(&tooldiscover.Result{}); err != nil {
		t.Fatalf("clean discovery reported %v", err)
	}
}
