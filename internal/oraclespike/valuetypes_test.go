package orabench

// What Go type does the driver hand back for each Oracle type?
//
// This is the measurement the SECOND ROW SHAPE needs. storm's port hands
// generated scanners raw wire bytes, and a database/sql driver decodes before
// storm can see them — so supporting one means generated code scanning
// driver.Value instead, and that only works if the mapping is known and
// LOSSLESS. A NUMBER(19,4) that arrives as a float64 is a money column with a
// rounding error in it, and no amount of careful scanning fixes that
// afterwards.
//
// Reported, not asserted: the output is the input to a design decision.

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
)

func TestWhatGoTypeEachOracleTypeArrivesAs(t *testing.T) {
	db := open(t)
	drop(db, "TABLE", `"vt_probe" CASCADE CONSTRAINTS PURGE`)
	mustExec(t, db, `CREATE TABLE "vt_probe" (
		"c_num19"    NUMBER(19),
		"c_num10"    NUMBER(10),
		"c_num5"     NUMBER(5),
		"c_money"    NUMBER(19,4),
		"c_numbare"  NUMBER,
		"c_bindbl"   BINARY_DOUBLE,
		"c_binflt"   BINARY_FLOAT,
		"c_vc"       VARCHAR2(40 CHAR),
		"c_clob"     CLOB,
		"c_raw"      RAW(16),
		"c_blob"     BLOB,
		"c_bool"     BOOLEAN,
		"c_date"     DATE,
		"c_ts"       TIMESTAMP(6),
		"c_tstz"     TIMESTAMP(6) WITH TIME ZONE,
		"c_json"     JSON)`)
	t.Cleanup(func() { drop(db, "TABLE", `"vt_probe" CASCADE CONSTRAINTS PURGE`) })

	mustExec(t, db, `INSERT INTO "vt_probe" VALUES (
		9007199254740993, 2000000000, 32000,
		12345.6789, 1234567890123456789012345678901234.5,
		1.25, 1.5,
		'text', 'clob text', HEXTORAW('0102030405060708090A0B0C0D0E0F10'),
		HEXTORAW('DEADBEEF'), TRUE,
		DATE '2026-01-02', TIMESTAMP '2026-01-02 03:04:05.123456',
		TIMESTAMP '2026-01-02 03:04:05.123456 +02:00', JSON('{"a":1}'))`)

	rows, err := db.Query(`SELECT * FROM "vt_probe"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	types, _ := rows.ColumnTypes()

	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	dest := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}

	t.Log("column          database type      Go type        value")
	for i, c := range cols {
		dbType := ""
		if types != nil && i < len(types) {
			dbType = types[i].DatabaseTypeName()
		}
		v := dest[i]
		t.Logf("  %-13s %-18s %-14s %v", c, dbType, reflect.TypeOf(v), truncate(v))
	}

	// The two that decide whether a value path can be LOSSLESS.
	//
	// A NUMBER(19) past 2^53 and a NUMBER(19,4) are exactly where a float64
	// stops being able to hold what the column does. If either arrives as a
	// float64 the second row shape needs a driver option or a per-column CAST,
	// and that is worth knowing before any of it is designed.
	for _, tc := range []struct {
		col string
		why string
	}{
		{"c_num19", "9007199254740993 is 2^53+1: a float64 cannot represent it"},
		{"c_money", "12345.6789 in a float64 is a money column with a rounding error"},
		{"c_numbare", "34 significant digits: nothing but text or a decimal holds this"},
	} {
		i := indexOfCol(cols, tc.col)
		if i < 0 {
			continue
		}
		t.Logf("DECISIVE  %s → %v   (%s)", tc.col, reflect.TypeOf(dest[i]), tc.why)
	}
}

// And the same question for the NULL case, because a driver that yields a typed
// zero rather than nil makes "absent" and "zero" the same value — the exact
// shape of the empty-string problem, one layer down.
func TestWhatANullArrivesAs(t *testing.T) {
	db := open(t)
	var n, s, tm any
	err := db.QueryRow(`SELECT CAST(NULL AS NUMBER(19)), CAST(NULL AS VARCHAR2(10)),`+
		` CAST(NULL AS TIMESTAMP(6)) FROM dual`).Scan(&n, &s, &tm)
	if err != nil {
		t.Fatalf("a row of three NULLs: %v", err)
	}
	t.Logf("NULL NUMBER    → %v (%v)", n, reflect.TypeOf(n))
	t.Logf("NULL VARCHAR2  → %v (%v)", s, reflect.TypeOf(s))
	t.Logf("NULL TIMESTAMP → %v (%v)", tm, reflect.TypeOf(tm))
	for name, v := range map[string]any{"NUMBER": n, "VARCHAR2": s, "TIMESTAMP": tm} {
		if v != nil {
			t.Errorf("a NULL %s arrived as %#v rather than nil; absent and zero would "+
				"be the same value", name, v)
		}
	}
}

func indexOfCol(cols []string, want string) int {
	for i, c := range cols {
		if c == want {
			return i
		}
	}
	return -1
}

func truncate(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 44 {
		return s[:41] + "..."
	}
	return s
}

var _ = sql.ErrNoRows
