package orabench

// Every statement compile/oracle can produce, PREPAREd and EXECUTEd.
//
// docs/PRODUCTION-READINESS.md P6.7: a test that does not execute against the
// target proves the generator is consistent with itself and nothing more. M9
// found twelve defects the first time its lowering met a server, and M10 found
// more; none was visible to a test that asserted on text.
//
// PREPARE alone would catch syntax. EXECUTE is what catches the rest — a
// collation name that parses and does not exist, a bind type Oracle will not
// compare, a LOB in a predicate it refuses.

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/oracle"
)

// loweringTables are created once for the whole file and dropped after.
const (
	loTable = "lo_probe"
	loChild = "lo_child"
)

func loweringSetup(t *testing.T) *sql.DB {
	t.Helper()
	db := open(t)
	for _, tb := range []string{loChild, loTable} {
		drop(db, "TABLE", `"`+tb+`" CASCADE CONSTRAINTS PURGE`)
	}
	mustExec(t, db, `CREATE TABLE "`+loTable+`" (
		"id" NUMBER(19) NOT NULL,
		"email" VARCHAR2(255 CHAR) NOT NULL,
		"rank" NUMBER(19) NOT NULL,
		"body" CLOB NOT NULL,
		"doc" JSON,
		"deleted_at" TIMESTAMP(6) WITH TIME ZONE,
		"updated_at" TIMESTAMP(6) WITH TIME ZONE NOT NULL,
		PRIMARY KEY ("id"))`)
	mustExec(t, db, `CREATE TABLE "`+loChild+`" (
		"id" NUMBER(19) NOT NULL,
		"parent_id" NUMBER(19) NOT NULL,
		"deleted_at" TIMESTAMP(6) WITH TIME ZONE,
		PRIMARY KEY ("id"))`)
	t.Cleanup(func() {
		for _, tb := range []string{loChild, loTable} {
			drop(db, "TABLE", `"`+tb+`" CASCADE CONSTRAINTS PURGE`)
		}
	})
	for i := 1; i <= 3; i++ {
		mustExec(t, db, `INSERT INTO "`+loTable+`" ("id","email","rank","body","doc","updated_at") `+
			`VALUES (:1,:2,:3,:4,JSON('{"a":1,"b":2}'),SYSTIMESTAMP)`,
			i, fmt.Sprintf("u%d@example.com", i), i*10, "body")
		mustExec(t, db, `INSERT INTO "`+loChild+`" ("id","parent_id") VALUES (:1,:2)`, i, i)
	}
	return db
}

// ran counts the statements that actually reached the server, so the gate can
// refuse to pass by not running — the discipline scripts/check/mssql.sh has.
type runner struct {
	db *sql.DB
	n  int
}

// exec EXECUTES one statement, and does not bother preparing it first.
//
// The first run of this gate DID prepare, and learned that go-ora's Prepare
// never reaches the server: two statements Oracle refuses outright — the row
// constructor and a capped locked read — prepared without error and then failed
// on execution. A PREPARE that does not round-trip proves nothing, which is the
// same shape of defect as a gate that passes by skipping.
func (r *runner) exec(t *testing.T, name, stmt string, args ...any) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		rows, err := r.db.Query(stmt, args...)
		if err != nil {
			t.Fatalf("refused:\n  %s\n  %v", stmt, err)
		}
		defer rows.Close()
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading the result:\n  %s\n  %v", stmt, err)
		}
	})
	r.n++
}

// refuses asserts the server rejects a statement, by EXECUTING it. Same reason.
func refuses(t *testing.T, db *sql.DB, name, stmt, wantORA string, args ...any) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		rows, err := db.Query(stmt, args...)
		if err == nil {
			rows.Close()
			t.Errorf("the server accepted this, so the refusal may be removable:\n  %s", stmt)
			return
		}
		t.Logf("confirmed: %v", firstLine(err))
		if wantORA != "" && !strings.Contains(err.Error(), wantORA) {
			t.Errorf("expected %s, got: %v", wantORA, err)
		}
	})
}

func TestEveryStatementTheLoweringProducesRuns(t *testing.T) {
	db := loweringSetup(t)
	r := &runner{db: db}

	tq := oracle.Ident(loTable)
	cols := []string{"id", "email", "rank"}
	sel := oracle.SelectPrefix(loTable, cols)

	// ---- the base read, in every shape ----
	r.exec(t, "base read", sel)
	r.exec(t, "count", oracle.CountPrefix(loTable))
	r.exec(t, "exists", oracle.ExistsPrefix(loTable)+oracle.ExistsSuffix())
	r.exec(t, "ordered", sel+oracle.OrderLead+oracle.DefaultOrderBy([]string{"id"}, "id"))
	// numbered, because a suffix carries the bare sigil the SPLICER numbers —
	// the same reason the operator fragments below go through it.
	r.exec(t, "capped", numbered(sel+oracle.OrderLead+`"id"`+oracle.LimitOffsetSuffix(false)), 2)
	r.exec(t, "paged", numbered(sel+oracle.OrderLead+`"id"`+oracle.LimitOffsetSuffix(true)), 1, 2)

	// Every ORDER BY direction, including the two placements neither MySQL nor
	// SQL Server can spell.
	for d := 0; d < oracle.NDirections; d++ {
		r.exec(t, fmt.Sprintf("order direction %d", d),
			sel+oracle.OrderLead+oracle.OrderTerm(d, `"deleted_at"`))
	}

	// ---- every operator fragment ----
	type opCase struct {
		op   string
		col  string
		args []any
	}
	for _, c := range []opCase{
		{"Eq", "rank", []any{10}},
		{"NotEq", "rank", []any{10}},
		{"Gt", "rank", []any{10}},
		{"Gte", "rank", []any{10}},
		{"Lt", "rank", []any{10}},
		{"Lte", "rank", []any{10}},
		{"Like", "email", []any{"u1%"}},
		{"ILike", "email", []any{"U1%"}},
		{"IsNull", "deleted_at", nil},
		{"IsNotNull", "updated_at", nil},
		{"EqLower", "email", []any{"u1@example.com"}},
	} {
		a, b, ok := oracle.Frag(c.op, tq+"."+oracle.Ident(c.col))
		if !ok {
			t.Errorf("%s has no lowering", c.op)
			continue
		}
		r.exec(t, "op "+c.op, sel+oracle.WhereLead+numbered(a+b), c.args...)
	}

	// The JSON key-presence operators are REFUSED, and this is the measurement
	// behind that. A path must be a literal here, so a key that arrives as a
	// bound value cannot be looked up — the first draft of compile/oracle
	// claimed the opposite and ORA-00907 corrected it.
	for _, op := range []string{"HasAnyKey", "HasAllKeys"} {
		if oracle.Supported(op) {
			t.Errorf("%s is claimed but JSON_EXISTS takes a literal path", op)
		}
	}
	refuses(t, db, "a JSON path built by concatenation",
		`SELECT 1 FROM `+tq+` WHERE JSON_EXISTS(`+tq+`."doc", '$.' || 'a')`, "ORA-00907")

	// ---- the IN list: one bound document, whatever the arity ----
	for _, tc := range []struct {
		name, col, colType, doc string
		negate                  bool
	}{
		{"IN on a number key", "rank", "NUMBER(19)", "[10,20]", false},
		{"NOT IN on a number key", "rank", "NUMBER(19)", "[10]", true},
		{"IN on a text key", "email", "VARCHAR2(4000)", `["u1@example.com"]`, false},
	} {
		a, b := oracle.InFrag(tq+"."+oracle.Ident(tc.col), tc.colType, tc.negate)
		r.exec(t, tc.name, sel+oracle.WhereLead+numbered(a+b), tc.doc)
	}

	// ---- the keyset comparison ----
	//
	// The constructor itself, because Oracle has one that compares
	// lexicographically — see the subtest below, which is what established
	// that and which is why RowCmpExpand is false on this target.
	keyset := sel + oracle.WhereLead +
		oracle.TupleOpen + tq + `."rank"` + oracle.TupleSep + tq + `."id"` + oracle.TupleClose +
		oracle.RowCmpOp(0) +
		oracle.TupleOpen + ":1" + oracle.TupleSep + ":2" + oracle.TupleClose +
		oracle.OrderLead + `"rank", "id"`
	r.exec(t, "keyset comparison", keyset, 10, 1)

	// And whether the row constructor is actually absent.
	//
	// The first run of this gate found that Oracle ACCEPTS the statement,
	// which would make RowCmpExpand unnecessary — so this asks the question
	// properly: not "does it parse" but "does it return the right rows". Rows
	// are (rank,id) = (10,1), (20,2), (30,3), so `> (10,1)` is two of them.
	// A constructor that parses and compares only the first column would give
	// the same answer for this data, so the second case uses a tie.
	t.Run("the row constructor", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			rank, id int
			wantRows int
		}{
			{"strictly greater", 10, 1, 2},
			{"a tie on the leading key", 20, 1, 2}, // (20,2) and (30,3)
		} {
			var n int
			err := db.QueryRow(
				`SELECT count(*) FROM `+tq+` WHERE ("rank", "id") > (:1, :2)`,
				tc.rank, tc.id).Scan(&n)
			if err != nil {
				t.Logf("%s: refused — %v", tc.name, firstLine(err))
				t.Log("RowCmpExpand is needed, which is what compile/oracle assumes")
				return
			}
			if n != tc.wantRows {
				t.Errorf("%s: the constructor parsed and returned %d rows, want %d — "+
					"it is not comparing the way a keyset filter needs", tc.name, n, tc.wantRows)
				return
			}
			t.Logf("%s: %d rows, correct", tc.name, n)
		}
		t.Log("Oracle's row constructor WORKS for inequality; RowCmpExpand may be " +
			"droppable, which would remove a whole expansion from this back end")
	})

	// ---- the three lock modes this target has ----
	for _, m := range []int{oracle.LockUpdate, oracle.LockUpdateNoWait, oracle.LockUpdateSkipLocked} {
		if why := oracle.LockRefused(m); why != "" {
			t.Errorf("mode %d should be supported: %s", m, why)
			continue
		}
		r.exec(t, "lock mode "+strings.TrimSpace(oracle.LockSuffix(m)),
			sel+oracle.WhereLead+tq+`."id" = :1`+oracle.LockSuffix(m), 1)
	}

	// And the combination that is ORA-02014, so LockRefusedCapped is a
	// measured refusal rather than a remembered one.
	refuses(t, db, "a capped locked read",
		numbered(sel+oracle.OrderLead+`"id"`+oracle.LimitOffsetSuffix(false)+
			oracle.LockSuffix(oracle.LockUpdateSkipLocked)), "ORA-02014", 1)

	// ---- the write path ----
	a, _ := oracle.NowFrag("updated_at")
	upd := oracle.UpdatePrefix(loTable) + a + oracle.WhereLead + oracle.Ident("id") + " = :1"
	if _, err := db.Exec(upd, 1); err != nil {
		t.Errorf("UPDATE refused:\n  %s\n  %v", upd, err)
	} else {
		r.n++
	}

	t.Logf("== %d statement(s) reached the server ==", r.n)
	if r.n < 29 {
		// A floor, not a target. It moves only when a statement is added or
		// removed on purpose — which has happened twice already, both times
		// because the server disagreed with the documentation.
		t.Errorf("only %d statements ran; the lowering produces more than that, so "+
			"something was skipped rather than exercised", r.n)
	}
}

// numbered assigns ordinals to the placeholders a fragment carries, which is
// the splicer's job in the generated package. Here it only has to be
// consistent: a fragment holds at most one.
func numbered(frag string) string {
	n := 0
	var b strings.Builder
	for i := 0; i < len(frag); i++ {
		if frag[i] == ':' && (i+1 >= len(frag) || frag[i+1] < '0' || frag[i+1] > '9') {
			n++
			b.WriteString(fmt.Sprintf(":%d", n))
			continue
		}
		b.WriteByte(frag[i])
	}
	return b.String()
}
