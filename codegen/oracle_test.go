package codegen

// The Oracle back end's wiring, and the row-shape choice that makes it work.
//
// A lowering with a nil field is a nil dereference during generation, and a
// back end added by copying another one is exactly where a field goes missing.
// This walks the struct rather than naming fields, so a NEW field added to the
// seam fails here for every dialect until each has filled it in.

import (
	"reflect"
	"strings"
	"testing"
)

// optional are the fields a back end may legitimately leave nil, each with the
// reason. Anything else nil is a hole.
var optional = map[string]string{
	"Upsert":                  "a back end whose upsert is a statement rather than a clause",
	"Merge":                   "a back end whose upsert IS a clause",
	"MergeOutput":             "ditto",
	"CreateIndexConcurrently": "a back end with no non-blocking index build",
	"DropIndexConcurrently":   "ditto",
	"LockHint":                "a back end whose lock is a suffix rather than a hint",
}

func TestEveryDialectFillsTheSeam(t *testing.T) {
	for _, d := range []Dialect{
		DialectPostgres, DialectMySQL, DialectMariaDB, DialectMSSQL, DialectOracle,
	} {
		l := loweringFor(d)
		v := reflect.ValueOf(l)
		rt := v.Type()
		for i := 0; i < rt.NumField(); i++ {
			f, name := v.Field(i), rt.Field(i).Name
			if f.Kind() != reflect.Func || !f.IsNil() {
				continue
			}
			if _, ok := optional[name]; ok {
				continue
			}
			t.Errorf("%s: %s is nil, which is a nil dereference during generation", d, name)
		}
		if l.name == "" {
			t.Errorf("%v has no name", d)
		}
	}
}

// The second row shape, chosen at GENERATE time. A generated Oracle package
// reads Values because a database/sql driver decodes before storm can see the
// wire; everything else reads RawValues because storm's own clients do not.
func TestOnlyOracleReadsTheValueShape(t *testing.T) {
	want := map[Dialect][2]string{
		DialectPostgres: {"[][]byte", "RawValues"},
		DialectMySQL:    {"[][]byte", "RawValues"},
		DialectMariaDB:  {"[][]byte", "RawValues"},
		DialectMSSQL:    {"[][]byte", "RawValues"},
		DialectOracle:   {"[]any", "Values"},
	}
	for d, w := range want {
		dec := decodersFor(d, "github.com/gsoultan/storm")
		if got := [2]string{dec.rowsType(), dec.rowsAccessor()}; got != w {
			t.Errorf("%s reads %v, want %v", d, got, w)
		}
		// And asks for the shape it reads. The value side is a second
		// interface, not a method every Rows has, so a value-shaped read that
		// skipped the ask would not compile — and a byte-shaped one that made
		// it would refuse storm's own clients.
		call := dec.rowsFrom("ex.Query(x)")
		param, convert := dec.batchRows()
		if w[1] == "Values" {
			if call != "runtime.AsValueRows(ex.Query(x))" || param != "r" ||
				convert != "rs, err := runtime.AsValueRows(r, err)" {
				t.Errorf("%s reads Values without asking for them: %q, %q, %q", d, call, param, convert)
			}
			continue
		}
		if call != "ex.Query(x)" || param != "rs" || convert != "" {
			t.Errorf("%s reads RawValues and must take Rows as they come: %q, %q, %q", d, call, param, convert)
		}
	}
}

// A uuid is `copy(r.F[:], rv[i])` where rv[i] is a []byte and a decoder call
// where it is an `any`. The hook is what keeps the two apart, and a family
// that lost it would emit code that does not compile — which is how it was
// found.
func TestTheValueFamilyHasItsOwnUUIDAndTextHooks(t *testing.T) {
	dec := decodersFor(DialectOracle, "github.com/gsoultan/storm")
	if dec.uuid == nil {
		t.Fatal("the value family needs a uuid hook: copy() wants a slice and rv[i] is an any")
	}
	if got := dec.uuid("ID", 0); !strings.Contains(got, "valdec.UUID") {
		t.Errorf("got %q", got)
	}
	if dec.text == nil {
		t.Fatal("the value family needs a text hook: the driver already allocated the string")
	}
	if got := dec.text("Name", 1); strings.Contains(got, "sl") {
		t.Errorf("a slab would be a second allocation for no lifetime benefit: %q", got)
	}
	// And the byte families keep the copy, so the hook is genuinely per-family.
	if decodersFor(DialectPostgres, "x").uuid != nil {
		t.Error("the byte families copy into the array directly")
	}
}

// Oracle cannot hand back the row it wrote — RETURNING binds output parameters
// the port does not carry — so keys are storm's to generate.
func TestOracleCannotReturnAndGeneratesItsOwnKeys(t *testing.T) {
	l := loweringFor(DialectOracle)
	if l.canReturn() {
		t.Error("Oracle's RETURNING binds OUTPUT parameters; runtime.Executor carries none")
	}
	if !l.KeysAreClientSide {
		t.Error("without a returning clause the key has to be storm's, as it is on MySQL")
	}
	if _, err := l.InsertStmt("t", []string{"a"}, []string{"a"}); err == nil {
		t.Error("a returning list must be refused rather than dropped")
	}
}
