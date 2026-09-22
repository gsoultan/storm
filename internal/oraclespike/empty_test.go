package orabench

// Result 3, and the one that decides M12: what exactly does
// "Oracle's empty string is NULL" break, and can storm's capability model
// carry it?
//
// docs/PLAN.md makes this the kill criterion — "capability model cannot carry
// Oracle → Mongo is cancelled" — so it is worth being precise about what the
// claim even is. Every refusal storm has today is SYNTACTIC: this construct has
// no form on that target, so the generator will not write it. A model-level
// Check answers "can this be expressed"; a lowering answers "can this be
// spelled". Empty-string-is-NULL is neither. It is a difference in what a
// statement MEANS once it runs, and the value it turns on arrives at runtime.
//
// docs/DIALECTS.md claims this is surfaced at declare time: "Declaring `oracle`
// in `portability.assert` makes any `Eq("")` or non-null-constrained text
// column a declare-time error." Half of that is impossible and the other half
// may be enough. See capability_test.go, which does not need a server, and the
// README, which puts the two halves together.

import (
	"database/sql"
	"testing"
)

func TestEmptyStringIsNull(t *testing.T) {
	db := open(t)
	drop(db, "TABLE", "e_probe PURGE")
	mustExec(t, db, `CREATE TABLE e_probe (id NUMBER PRIMARY KEY, txt VARCHAR2(40), req VARCHAR2(40) NOT NULL)`)
	t.Cleanup(func() { drop(db, "TABLE", "e_probe PURGE") })

	// 1. The headline. A written empty string comes back as NULL.
	mustExec(t, db, `INSERT INTO e_probe (id, txt, req) VALUES (1, '', 'x')`)
	var isNull bool
	if err := db.QueryRow(
		`SELECT CASE WHEN txt IS NULL THEN 1 ELSE 0 END FROM e_probe WHERE id = 1`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	t.Logf(`INSERT '' into a nullable VARCHAR2 → IS NULL: %v`, isNull)
	if !isNull {
		t.Error("DECISION-CRITICAL: the premise of this whole milestone is wrong, " +
			"which would be good news")
	}

	// 2. And it comes back through a BOUND parameter the same way, which is
	//    the path storm would actually use. A driver that turned "" into
	//    something else would hide the difference rather than fix it.
	mustExec(t, db, `INSERT INTO e_probe (id, txt, req) VALUES (2, :1, 'x')`, "")
	var got sql.NullString
	if err := db.QueryRow(`SELECT txt FROM e_probe WHERE id = 2`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	t.Logf(`bound "" → Valid=%v String=%q`, got.Valid, got.String)
	if got.Valid {
		t.Error("a bound empty string survived as a value; the driver is normalising it")
	}

	// 3. THE LOAD-BEARING ONE. Can an empty string reach a NOT NULL column?
	//
	//    If it CANNOT, the difference stops being silent: on Oracle an
	//    application that writes "" to a required column gets ORA-01400, and
	//    an error is a thing a caller can see. Silence is what cannot be
	//    shipped. This is the single result the capability question turns on.
	_, err := db.Exec(`INSERT INTO e_probe (id, txt, req) VALUES (3, 'y', '')`)
	t.Logf(`INSERT '' into a NOT NULL VARCHAR2 → %v`, err)
	if err == nil {
		t.Error("DECISION-CRITICAL: '' was accepted by a NOT NULL column, so the " +
			"difference is SILENT there and refusing nullable text is not enough")
	}

	// 4. And a predicate against it matches nothing, because '' is NULL and
	//    NULL = NULL is unknown. Consistent with 3: if nothing can BE '', then
	//    nothing matching '' is the right answer rather than a wrong one.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM e_probe WHERE req = ''`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Logf(`WHERE req = '' → %d row(s)`, n)
	if n != 0 {
		t.Errorf("expected 0 rows, got %d", n)
	}

	// 5. THE LEAK. An empty string produced by an EXPRESSION is not a value
	//    anybody declared, so no column rule can refuse it. If SUBSTR of zero
	//    length is NULL here and '' on PostgreSQL, then any lowering that
	//    emits a string function has a semantic difference no Check can see.
	//
	//    A second, non-NULL column on purpose. The first run of this probe
	//    asked for the expression alone and got "sql: no rows in result set"
	//    from a SELECT against dual, which cannot return none — so the
	//    question of whether Oracle returns a row and whether the DRIVER hands
	//    it over are two questions, and they are separated here.
	var sub sql.NullString
	var guard int
	if err := db.QueryRow(
		`SELECT SUBSTR('abc', 1, 0), 1 FROM dual`).Scan(&sub, &guard); err != nil {
		t.Logf("SUBSTR probe: %v", err)
	} else {
		t.Logf(`SUBSTR('abc',1,0) → Valid=%v String=%q  (PostgreSQL: Valid=true "")`, sub.Valid, sub.String)
	}

	// The same value as the ONLY column, which is what failed before. If this
	// errors while the two-column form works, the finding is about go-ora and
	// not about Oracle — and it would be a finding about the driver storm was
	// considering adopting.
	var alone sql.NullString
	err = db.QueryRow(`SELECT SUBSTR('abc', 1, 0) FROM dual`).Scan(&alone)
	t.Logf(`the same expression as the only column → err=%v Valid=%v`, err, alone.Valid)

	var lengthOfEmpty sql.NullInt64
	if err := db.QueryRow(`SELECT LENGTH(''), 1 FROM dual`).Scan(&lengthOfEmpty, &guard); err != nil {
		// A FINDING about go-ora, not about Oracle. Logged rather than failed:
		// see the README's driver section.
		t.Logf("LENGTH probe: %v  ← go-ora, not Oracle", err)
	} else {
		t.Logf(`LENGTH('') → Valid=%v Int64=%d  (PostgreSQL: Valid=true 0)`,
			lengthOfEmpty.Valid, lengthOfEmpty.Int64)
	}

	// 6. Concatenation does NOT propagate the NULL, which is the one place
	//    Oracle is friendlier than the standard — and is why the difference is
	//    easy to miss in testing.
	var cat string
	if err := db.QueryRow(`SELECT '' || 'x' FROM dual`).Scan(&cat); err != nil {
		t.Fatal(err)
	}
	t.Logf(`'' || 'x' → %q  (standard NULL propagation would give NULL)`, cat)
}

// The round trip storm actually promises: write a Go "" through a bound
// parameter, read it back into a Go string.
//
// On PostgreSQL, MySQL and SQL Server this is the identity. Whatever it is
// here is what a model with a nullable text column would give its caller on
// Oracle, and it is the concrete form of "the capability model has to carry
// this or refuse it".
func TestNullableTextDoesNotRoundTrip(t *testing.T) {
	db := open(t)
	drop(db, "TABLE", "r_probe PURGE")
	mustExec(t, db, `CREATE TABLE r_probe (id NUMBER PRIMARY KEY, txt VARCHAR2(40))`)
	t.Cleanup(func() { drop(db, "TABLE", "r_probe PURGE") })

	mustExec(t, db, `INSERT INTO r_probe (id, txt) VALUES (1, :1)`, "")
	mustExec(t, db, `INSERT INTO r_probe (id, txt) VALUES (2, :1)`, nil)

	for _, id := range []int{1, 2} {
		var v sql.NullString
		if err := db.QueryRow(`SELECT txt FROM r_probe WHERE id = :1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		t.Logf("id=%d wrote %s, read Valid=%v", id, map[int]string{1: `""`, 2: "NULL"}[id], v.Valid)
	}

	// Two DIFFERENT writes, one indistinguishable result. A `*string` field in
	// a storm model means exactly this distinction, so on Oracle it is a field
	// whose type promises something the database cannot keep.
	var distinct int
	if err := db.QueryRow(
		`SELECT count(DISTINCT nvl(txt, '<null>')) FROM r_probe`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	t.Logf(`"" and NULL are %d distinct value(s) in the catalogue's eyes`, distinct)
	if distinct != 1 {
		t.Errorf("DECISION-CRITICAL: expected them to collapse to 1, got %d — "+
			"if they do not collapse, a nullable text column needs no refusal", distinct)
	}
}
