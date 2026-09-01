package schema_test

import (
	"testing"

	"github.com/gsoultan/storm/schema"
)

// The result type of an aggregate is NOT its input type, and the difference is
// invisible until a decode reads the wrong bytes. Every row here is a fact
// about PostgreSQL, asserted so a future edit has to argue with it.
func TestAggregateResultTypes(t *testing.T) {
	cases := []struct {
		fn       schema.AggFunc
		in       string
		want     string
		nullable bool
	}{
		// count is the only one that cannot be NULL: no rows is 0 rows.
		{schema.AggCount, schema.TypeInt4, schema.TypeInt8, false},
		{schema.AggCount, schema.TypeText, schema.TypeInt8, false},

		// sum widens, because PostgreSQL will not risk overflowing the input.
		{schema.AggSum, schema.TypeInt2, schema.TypeInt8, true},
		{schema.AggSum, schema.TypeInt4, schema.TypeInt8, true},
		{schema.AggSum, schema.TypeInt8, schema.TypeNumeric, true},
		{schema.AggSum, schema.TypeNumeric, schema.TypeNumeric, true},
		{schema.AggSum, schema.TypeFloat4, schema.TypeFloat4, true},
		{schema.AggSum, schema.TypeFloat8, schema.TypeFloat8, true},

		// an average is not an integer, ever.
		{schema.AggAvg, schema.TypeInt2, schema.TypeNumeric, true},
		{schema.AggAvg, schema.TypeInt4, schema.TypeNumeric, true},
		{schema.AggAvg, schema.TypeInt8, schema.TypeNumeric, true},
		{schema.AggAvg, schema.TypeNumeric, schema.TypeNumeric, true},
		// float4 accumulates in double precision.
		{schema.AggAvg, schema.TypeFloat4, schema.TypeFloat8, true},
		{schema.AggAvg, schema.TypeFloat8, schema.TypeFloat8, true},

		// min/max preserve the type exactly.
		{schema.AggMin, schema.TypeText, schema.TypeText, true},
		{schema.AggMax, schema.TypeTimestamptz, schema.TypeTimestamptz, true},
		{schema.AggMin, schema.TypeUUID, schema.TypeUUID, true},
	}
	for _, c := range cases {
		got, nullable, err := schema.AggregateResult(c.fn, schema.Type{Name: c.in})
		if err != nil {
			t.Errorf("%s(%s): %v", c.fn, c.in, err)
			continue
		}
		if got.Name != c.want {
			t.Errorf("%s(%s) = %s, want %s", c.fn, c.in, got.Name, c.want)
		}
		if nullable != c.nullable {
			t.Errorf("%s(%s) nullable = %v, want %v", c.fn, c.in, nullable, c.nullable)
		}
	}
}

// min/max preserve precision and scale, which a Decimal decode depends on.
func TestMinMaxPreservesNumericPrecision(t *testing.T) {
	in := schema.Type{Name: schema.TypeNumeric, Precision: 12, Scale: 2}
	got, _, err := schema.AggregateResult(schema.AggMax, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Precision != 12 || got.Scale != 2 {
		t.Errorf("max(numeric(12,2)) = numeric(%d,%d), want (12,2)", got.Precision, got.Scale)
	}
}

// sum(numeric) must NOT carry the declared precision forward: the sum needs
// more digits than any single row, and pretending otherwise would let a
// precision check pass on a value that cannot fit.
func TestSumNumericIsUnconstrained(t *testing.T) {
	in := schema.Type{Name: schema.TypeNumeric, Precision: 12, Scale: 2}
	got, _, err := schema.AggregateResult(schema.AggSum, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Precision != 0 {
		t.Errorf("sum(numeric(12,2)) carried precision %d forward", got.Precision)
	}
}

func TestAggregateResultRejectsNonsense(t *testing.T) {
	for _, fn := range []schema.AggFunc{schema.AggSum, schema.AggAvg} {
		if _, _, err := schema.AggregateResult(fn, schema.Type{Name: schema.TypeText}); err == nil {
			t.Errorf("%s(text) was accepted", fn)
		}
	}
	if _, _, err := schema.AggregateResult(schema.AggFunc("median"), schema.Type{Name: schema.TypeInt4}); err == nil {
		t.Error("an unknown aggregate was accepted")
	}
}
