package codegen_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

type bgTicket struct {
	storm.Model
	Ref      string
	Priority int16
	Seat     int32
}

// A column whose exported name is one the generated package already declares
// used to produce a file that does not compile, and the error named a line of
// generated code rather than the model that caused it.
type bgClash struct {
	storm.Model
	And string
}

func gen(t *testing.T, o codegen.PackageOptions, models ...any) map[string]string {
	t.Helper()
	s, err := storm.Build(models...)
	if err != nil {
		t.Fatal(err)
	}
	o.Import = "github.com/gsoultan/storm"
	if o.Dir == "" {
		o.Dir = "gen"
	}
	files, err := codegen.Package(s, o)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, src := range files {
		out[name] = string(src)
	}
	return out
}

func find(t *testing.T, files map[string]string, part string) string {
	t.Helper()
	for name, src := range files {
		if strings.Contains(name, part) {
			return src
		}
	}
	t.Fatalf("no generated file for %s; the test asserts nothing", part)
	return ""
}

// The default is the measured one, and stays byte-for-byte what it was: a
// knob whose zero value changes the output would have rewritten every
// generated tree in the repository.
func TestBudgets_ZeroValueIsTheMeasuredDefault(t *testing.T) {
	a := find(t, gen(t, codegen.PackageOptions{}, &bgTicket{}), "bgticket")
	b := find(t, gen(t, codegen.PackageOptions{Budgets: codegen.Budgets{Scale: 1}}, &bgTicket{}), "bgticket")
	if a != b {
		t.Fatal("Scale 1 and the zero value generate different code")
	}
	// gofmt aligns struct fields, so the declarations are matched by pattern
	// rather than by a literal that a widened column would break.
	for _, want := range []string{
		`toks +\[16\]runtime\.Tok`,
		`otoks +\[4\]runtime\.Tok`,
		`strs +\[6\]string`,
	} {
		if !regexp.MustCompile(want).MatchString(a) {
			t.Errorf("default output lost %q", want)
		}
	}
}

// The buffers were previously reachable only by editing storm's own source,
// which is not a knob an adopter has: a filter screen past sixteen predicate
// nodes could only be split.
func TestBudgets_ScaleRaisesEveryBuffer(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{Budgets: codegen.Budgets{Scale: 2}}, &bgTicket{}), "bgticket")
	for _, want := range []string{
		`toks +\[32\]runtime\.Tok`, // predicates
		`otoks +\[8\]runtime\.Tok`, // ordering
		`strs +\[12\]string`,       // a scalar arena
		`anyI16 +\[6\]\[\]int16`,   // a list arena
		`buf \[41\]runtime\.Tok`,   // the stream scratch: 32 + 8 + 1
	} {
		if !regexp.MustCompile(want).MatchString(src) {
			t.Errorf("Scale 2 did not produce %q", want)
		}
	}
	// The bind loop and the buffers have to agree, or a query overflows one
	// while the other still has room.
	if !strings.Contains(src, "make([]any, 0, 33)") {
		t.Error("the binder was not scaled with the token buffer")
	}
}

// An error that says "raise the limits in codegen" sends the reader into
// storm's source. It has to name the setting instead, and the value it was
// generated with, or they have to go and find that too.
func TestBudgets_OverflowErrorNamesTheOption(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{Budgets: codegen.Budgets{Scale: 3}}, &bgTicket{}), "bgticket")
	for _, want := range []string{"scale 3", "codegen.Budgets{Scale}"} {
		if !strings.Contains(src, want) {
			t.Errorf("the overflow error does not mention %q", want)
		}
	}
}

func TestReservedColumnNameIsRefused(t *testing.T) {
	s, err := storm.Build(&bgClash{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = codegen.Package(s, codegen.PackageOptions{
		Dir: "gen", Import: "github.com/gsoultan/storm",
	})
	if err == nil {
		t.Fatal("accepted a column named And; the package redeclares the function")
	}
	for _, want := range []string{"bg_clashes", "and", "And"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
