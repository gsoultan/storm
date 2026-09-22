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

// exec PREPAREs and EXECUTEs one statement. Both, because prepare alone catches
// syntax and nothing else: a collation that parses and does not exist, or a
// bind Oracle will not compare, only fails on execution.
func (r *runner) exec(t *testing.T, name, stmt string, args ...any) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		st, err := r.db.Prepare(stmt)
		if err != nil {
			t.Fatalf("PREPARE refused:\n  %s\n  %v", stmt, err)
		}
		defer st.Close()
		rows, err := st.Query(args...)
		if err != nil {
			t.Fatalf("EXECUTE refused:\n  %s\n  %v", stmt, err)
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
	r.exec(t, "capped", sel+oracle.OrderLead+`"id"`+oracle.LimitOffsetSuffix(false), 2)
	r.exec(t, "paged", sel+oracle.OrderLead+`"id"`+oracle.LimitOffsetSuffix(true), 1, 2)

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

	// The JSON operators, which are the one group richer here than on SQL
	// Server: HasAllKeys is expressible because the bound list is mentioned
	// once.
	for _, op := range []string{"HasAnyKey", "HasAllKeys"} {
		a, b, ok := oracle.Frag(op, tq+"."+oracle.Ident("doc"))
		if !ok {
			t.Errorf("%s has no lowering", op)
			continue
		}
		r.exec(t, "op "+op, sel+oracle.WhereLead+numbered(a+b), `["a","b"]`)
	}

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

	// ---- the expanded keyset comparison ----
	//
	// (a, b) > (:1, :2) is ORA-00920 here, so this is the OR-chain it means —
	// written the way runtime.expandRowCmp writes it, because a shape that
	// parses in a test and not in the generated package is worth nothing.
	keyset := sel + oracle.WhereLead + "(" +
		tq + `."rank" > :1 OR (` + tq + `."rank" = :2 AND ` + tq + `."id" > :3))` +
		oracle.OrderLead + `"rank", "id"`
	r.exec(t, "expanded keyset comparison", keyset, 10, 10, 1)

	// And the form that does NOT parse, so the refusal is not folklore.
	t.Run("the row constructor Oracle lacks", func(t *testing.T) {
		_, err := db.Prepare(sel + ` WHERE ("rank", "id") > (:1, :2)`)
		if err == nil {
			t.Error("(a,b) > (:1,:2) parsed; RowCmpExpand may not be needed after all")
		} else {
			t.Logf("confirmed: %v", firstLine(err))
		}
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
	t.Run("a capped locked read is refused by the server", func(t *testing.T) {
		_, err := db.Prepare(sel + oracle.OrderLead + `"id"` +
			oracle.LimitOffsetSuffix(false) + oracle.LockSuffix(oracle.LockUpdateSkipLocked))
		if err == nil {
			t.Error("FETCH FIRST with FOR UPDATE prepared; the refusal may be removable")
		} else {
			t.Logf("confirmed: %v", firstLine(err))
			if !strings.Contains(err.Error(), "ORA-02014") {
				t.Errorf("expected ORA-02014, which is what LockRefusedCapped names: %v", err)
			}
		}
	})

	// ---- the write path ----
	a, _ := oracle.NowFrag("updated_at")
	upd := oracle.UpdatePrefix(loTable) + a + oracle.WhereLead + oracle.Ident("id") + " = :1"
	if _, err := db.Exec(upd, 1); err != nil {
		t.Errorf("UPDATE refused:\n  %s\n  %v", upd, err)
	} else {
		r.n++
	}

	t.Logf("== %d statement(s) reached the server ==", r.n)
	if r.n < 30 {
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
