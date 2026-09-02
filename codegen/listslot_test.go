package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// Fixtures for the list-slot rule: one table with integer columns of all three
// widths, one with none.
type lsTicket struct {
	storm.Model
	Ref      string
	Priority int16
	Seat     int32
	Serial   int64
}

type lsVenue struct {
	storm.Model
	Name string
	Slug string
}

// A list slot costs 24 bytes in Pred and 24 more in Query, and Query is copied
// on every builder call — which is why bench carries a size tripwire at all.
// The slots have to stay conditional on the column kinds a table actually has:
// a table of text and uuid must not carry the machinery for integer lists just
// because integers can have them somewhere else. That is the property the byte
// count in bench is a proxy for, asserted directly here.
func TestListSlotsAreConditional(t *testing.T) {
	s, err := storm.Build(&lsTicket{}, &lsVenue{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:    "gen",
		Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}

	find := func(part string) string {
		t.Helper()
		for name, src := range files {
			if strings.Contains(name, part) {
				return string(src)
			}
		}
		t.Fatalf("no generated file for %s; the test asserts nothing", part)
		return ""
	}

	// lsVenue is uuid + text + timestamptz. No integer column anywhere.
	venue := find("lsvenue")
	for _, slot := range []string{"anyI16", "anyI32", "anyI64"} {
		if strings.Contains(venue, slot) {
			t.Errorf("lsVenue carries %s, but it has no integer column", slot)
		}
	}
	// The slots it should have, so this cannot pass by generating nothing.
	for _, slot := range []string{"anyRaw", "anyStr"} {
		if !strings.Contains(venue, slot) {
			t.Errorf("lsVenue is missing %s, which its uuid and text columns need", slot)
		}
	}

	// lsTicket has all three widths, each in its own slot: binding an int16
	// list as int64 would hand PostgreSQL an int8[] to compare against an int2
	// column, a cast it must undo before it can use an index.
	ticket := find("lsticket")
	for _, slot := range []string{"anyI16", "anyI32", "anyI64"} {
		if !strings.Contains(ticket, slot) {
			t.Errorf("lsTicket is missing %s", slot)
		}
	}
	for _, m := range []string{
		"func (h Int16Col) In(v ...int16) Pred",
		"func (h Int32Col) In(v ...int32) Pred",
		"func (h Int64Col) In(v ...int64) Pred",
		"func (h Int64Col) NotIn(v ...int64) Pred",
		"func (h TextCol) ILike(v string) Pred",
	} {
		if !strings.Contains(ticket, m) {
			t.Errorf("missing generated method: %s", m)
		}
	}
}

// A list value lands in an arena at its slot's cursor, exactly like a scalar.
// It used to be a single field, so a second list predicate on the same slot
// overwrote the first and the statement bound one list twice — wrong rows, no
// error. The bind side has to walk the same cursor, so both halves are checked
// here rather than trusting them to stay in step.
func TestListBindReachesEverySlot(t *testing.T) {
	s, err := storm.Build(&lsTicket{}, &lsVenue{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: "gen", Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}
	var src string
	for name, f := range files {
		if strings.Contains(name, "lsticket") {
			src = string(f)
		}
	}
	for _, sl := range []struct{ slot, cursor string }{
		{"anyRaw", "nar"}, {"anyStr", "nas"},
		{"anyI16", "nai16"}, {"anyI32", "nai32"}, {"anyI64", "nai64"},
	} {
		idx := "[" + sl.cursor + "]"
		if !strings.Contains(src, "b."+sl.slot+idx+" = q."+sl.slot+idx) {
			t.Errorf("the bind chain never assigns %s through its cursor", sl.slot)
		}
		if !strings.Contains(src, "v = append(v, &b."+sl.slot+idx+")") {
			t.Errorf("the bind chain never appends %s", sl.slot)
		}
		if !strings.Contains(src, sl.cursor+"++") {
			t.Errorf("the bind chain never advances %s; every list would bind the first value", sl.cursor)
		}
		// The build side must bound the arena. Without this a third list
		// predicate writes past the end.
		if !strings.Contains(src, "if int(q."+sl.cursor+") >= 3 {") {
			t.Errorf("%s is written without a bound check", sl.slot)
		}
	}
}
