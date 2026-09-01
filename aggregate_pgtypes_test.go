package storm_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm/schema"
	"github.com/jackc/pgx/v5"
)

// schema.AggregateResult encodes what PostgreSQL returns for each aggregate.
// Getting it wrong does not fail — it decodes the wrong bytes into the right
// looking field. So the table is not asserted from memory or from the docs; it
// is asserted against a live server, and this test is what stops it drifting
// when a future PostgreSQL changes one.
func TestAggregateResultMatchesPostgres(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	const tbl = "storm_aggtypes_probe"
	if _, err := c.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+`; CREATE TABLE `+tbl+`(
		i2 int2, i4 int4, i8 int8, n numeric(12,2), f4 float4, f8 float8,
		t text, ts timestamptz, u uuid)`); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(ctx, "DROP TABLE IF EXISTS "+tbl)

	// storm's canonical type name for what PostgreSQL calls each of these.
	pgName := map[string]string{
		schema.TypeInt8:        "bigint",
		schema.TypeNumeric:     "numeric",
		schema.TypeFloat4:      "real",
		schema.TypeFloat8:      "double precision",
		schema.TypeText:        "text",
		schema.TypeTimestamptz: "timestamp with time zone",
		schema.TypeUUID:        "uuid",
	}
	cases := []struct {
		fn  schema.AggFunc
		col string
		in  string
	}{
		{schema.AggSum, "i2", schema.TypeInt2}, {schema.AggSum, "i4", schema.TypeInt4},
		{schema.AggSum, "i8", schema.TypeInt8}, {schema.AggSum, "n", schema.TypeNumeric},
		{schema.AggSum, "f4", schema.TypeFloat4}, {schema.AggSum, "f8", schema.TypeFloat8},
		{schema.AggAvg, "i2", schema.TypeInt2}, {schema.AggAvg, "i4", schema.TypeInt4},
		{schema.AggAvg, "i8", schema.TypeInt8}, {schema.AggAvg, "n", schema.TypeNumeric},
		{schema.AggAvg, "f4", schema.TypeFloat4}, {schema.AggAvg, "f8", schema.TypeFloat8},
		{schema.AggMin, "t", schema.TypeText}, {schema.AggMax, "ts", schema.TypeTimestamptz},
		{schema.AggMax, "n", schema.TypeNumeric},
		{schema.AggCount, "t", schema.TypeText},
	}
	for _, c2 := range cases {
		want, _, err := schema.AggregateResult(c2.fn, schema.Type{Name: c2.in})
		if err != nil {
			t.Errorf("%s(%s): %v", c2.fn, c2.in, err)
			continue
		}
		var got string
		q := "SELECT pg_typeof(" + string(c2.fn) + "(" + c2.col + "))::text FROM " + tbl
		if err := c.QueryRow(ctx, q).Scan(&got); err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if got != pgName[want.Name] {
			t.Errorf("%s(%s): storm says %s (%q), PostgreSQL says %q",
				c2.fn, c2.in, want.Name, pgName[want.Name], got)
		}
	}

	// uuid and bool LOOK orderable and have no min/max aggregate. storm must
	// refuse them at declaration time rather than emit SQL that cannot run.
	for _, name := range []string{schema.TypeUUID, schema.TypeBool, schema.TypeJSONB, schema.TypeBytea} {
		if schema.AggregatableMinMax(schema.Type{Name: name}) {
			t.Errorf("storm allows min/max on %s, which PostgreSQL has no aggregate for", name)
		}
	}
	for _, name := range []string{schema.TypeText, schema.TypeTimestamptz, schema.TypeNumeric} {
		if !schema.AggregatableMinMax(schema.Type{Name: name}) {
			t.Errorf("storm refuses min/max on %s, which PostgreSQL supports", name)
		}
	}

	// The nullability half, which is the one that turns into a wrong answer:
	// over zero rows every aggregate but count returns NULL.
	var sumNull, avgNull, minNull, countNull bool
	var count int64
	if err := c.QueryRow(ctx, `SELECT sum(i4) IS NULL, avg(i4) IS NULL, min(t) IS NULL,
		count(*) IS NULL, count(*) FROM `+tbl).Scan(&sumNull, &avgNull, &minNull, &countNull, &count); err != nil {
		t.Fatal(err)
	}
	if !sumNull || !avgNull || !minNull {
		t.Error("an aggregate storm marks nullable did not return NULL over zero rows")
	}
	if countNull || count != 0 {
		t.Errorf("count over zero rows: null=%v value=%d, want false/0", countNull, count)
	}
	for _, fn := range []schema.AggFunc{schema.AggSum, schema.AggAvg, schema.AggMin, schema.AggMax} {
		if _, nullable, _ := schema.AggregateResult(fn, schema.Type{Name: schema.TypeInt4}); !nullable {
			t.Errorf("storm marks %s NOT NULL, but it is NULL over zero rows", fn)
		}
	}
	if _, nullable, _ := schema.AggregateResult(schema.AggCount, schema.Type{Name: schema.TypeInt4}); nullable {
		t.Error("storm marks count nullable, but it returns 0 over zero rows")
	}
}
