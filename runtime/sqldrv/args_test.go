package sqldrv

// database/sql takes seven kinds and storm's binders produce richer ones, so
// this conversion is load-bearing rather than defensive — and one case of it
// was worse than an error before it existed.

import (
	"reflect"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// THE case. go-ora reads an ARRAY-typed argument as a request for array
// binding and answers any insert carrying one with "to activate bulk
// insert/merge all parameters should be arrays" — so a [16]byte key, which
// every storm model has, made every insert fail. Found by running the
// generated package, not by reading the driver.
func TestAUUIDBecomesAByteSlice(t *testing.T) {
	var u [16]byte
	u[15] = 7
	got := normalize([]any{u})
	b, ok := got[0].([]byte)
	if !ok {
		t.Fatalf("a [16]byte stayed %T; go-ora reads that as a bulk-insert request", got[0])
	}
	if len(b) != 16 || b[15] != 7 {
		t.Errorf("the key did not survive: %v", b)
	}
}

// Through its decimal STRING, never a float64 — the same rule valdec reads by
// in the other direction. An Oracle NUMBER is exact text on the wire both ways.
func TestADecimalGoesOutAsText(t *testing.T) {
	d, err := runtime.ParseDecimal("12345.6789")
	if err != nil {
		t.Fatal(err)
	}
	got := normalize([]any{d})
	if s, ok := got[0].(string); !ok || s != "12345.6789" {
		t.Errorf("got %#v, want the string 12345.6789", got[0])
	}
}

func TestNarrowIntegersWiden(t *testing.T) {
	got := normalize([]any{int32(7), int16(8), int(9), uint8(10)})
	for i, v := range got {
		if _, ok := v.(int64); !ok {
			t.Errorf("argument %d is %T, and database/sql takes int64", i, v)
		}
	}
}

func TestANilPointerIsNULL(t *testing.T) {
	var p *string
	if got := normalize([]any{p}); got[0] != nil {
		t.Errorf("a nil pointer must bind as NULL, got %#v", got[0])
	}
	s := "x"
	if got := normalize([]any{&s}); got[0] != "x" {
		t.Errorf("a pointer must bind its value, got %#v", got[0])
	}
}

// The common case allocates nothing: a statement whose arguments are already
// driver values gets its own slice back.
func TestAlreadyDriverValuesAreNotCopied(t *testing.T) {
	args := []any{int64(1), "a", []byte{2}, true, 1.5, time.Now(), nil}
	got := normalize(args)
	if &got[0] != &args[0] {
		t.Error("a slice of driver values was copied for nothing")
	}
	if !reflect.DeepEqual(got, args) {
		t.Error("driver values were altered")
	}
}
