package msbench

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/runtime"
)

// Every statement compile/mssql lowers, PREPAREd and EXECUTEd by a real server.
//
// docs/PRODUCTION-READINESS.md §P6.7, from M9: "a test that does not EXECUTE
// against the target proves the generator is consistent with itself and nothing
// more." Twelve defects came out of running MySQL's lowering for the first
// time, two of which were PostgreSQL bugs. This is the same gate for M10, and
// it runs BEFORE the driver exists — the SQL is the half that can be proven
// with a borrowed client, so it is proven first.
//
// PREPARE is not enough on its own and EXECUTE is not either. sp_prepare types
// every parameter and parses the whole statement, which catches a syntax error
// and a column that is not there; executing catches the conversions that only
// fail on a value. Both, for every statement.

// lowered is one construct and the SQL it produced.
type lowered struct {
	name string
	sql  string
	args []any
}

// The gate's column ids, in the order the fragment table below numbers them.
const (
	colID = iota
	colEmail
	colRank
	colOrgID
	colDeleted
)

var gateCols = []string{"id", "email", "rank", "org_id", "deleted_at"}

// gateLowering is the runtime.Lowering a generated MSSQL package would carry.
//
// Built by hand here rather than taken from codegen, because codegen cannot
// emit a compiling SQL Server package until the decoder family exists — and
// waiting for that to find out whether the SQL parses is exactly the mistake
// this gate exists to avoid. The fields are assigned from compile/mssql, so
// what is exercised is the lowering rather than a restatement of it.
func gateLowering() runtime.Lowering {
	ident := func(col uint32) string { return mssql.Ident(gateCols[col]) }
	return runtime.Lowering{
		Frag: func(op, col uint32) runtime.Frag {
			a, b, ok := mssql.Frag(fragOps[op], ident(col))
			if !ok {
				return runtime.Frag{}
			}
			return runtime.Frag{A: a, B: b}
		},
		Order: func(dir, col uint32) string {
			return mssql.OrderTerm(int(dir), ident(col))
		},
		OB:            runtime.Order{Lead: mssql.OrderLead, Sep: mssql.OrderSep},
		Ident:         ident,
		RowCmp:        func(op uint32) string { return mssql.RowCmpOp(int(op)) },
		TupleOpen:     mssql.TupleOpen,
		TupleSep:      mssql.TupleSep,
		TupleClose:    mssql.TupleClose,
		RowCmpExpand:  mssql.RowCmpExpand,
		OrderFallback: mssql.OrderFallback,
		Placeholder:   runtime.MSSQLPlaceholder,
	}
}

var fragOps = []string{"Eq", "Gt", "Like", "IsNull", "In", "EqLower"}

const (
	opEq = iota
	opGt
	opLike
	opIsNull
	_ // In is not in the plain table; see inFrag
	opEqLower
)

func TestEveryLoweredStatementRunsOnTheServer(t *testing.T) {
	db := open(t)
	setup(t, db)

	lw := gateLowering()
	const table = "ms_members"
	readCols := []string{"id", "email", "rank", "org_id"}
	live := mssql.Live(mssql.LiveFor("", "deleted_at"))

	selectPrefix := mssql.SelectPrefix(table, readCols)
	limit := mssql.LimitOffsetSuffix(false)
	limitOffset := mssql.LimitOffsetSuffix(true)

	var cases []lowered
	add := func(name, sql string, args ...any) {
		cases = append(cases, lowered{name, sql, args})
	}

	// --- the base read, in every shape stmtFor can build ---------------------

	eq := []runtime.Tok{runtime.MakeLeaf(opEq, colEmail)}
	add("read: one predicate, capped",
		runtime.SpliceTree(selectPrefix, eq, lw, limit).SQL, "a@b.c", 10)

	add("read: predicate, ordered, paged",
		runtime.SpliceTree(selectPrefix,
			append(append([]runtime.Tok{}, eq...), runtime.MakeOrder(1, colRank)),
			lw, limitOffset).SQL, "a@b.c", 5, 10)

	// No ordering at all: OFFSET/FETCH is a clause OF ORDER BY, so this is the
	// case that is a syntax error without OrderFallback.
	add("read: no ordering, capped",
		runtime.SpliceTree(selectPrefix, eq, lw, limit).SQL, "a@b.c", 10)

	// Every lock mode, as a table hint on the FROM rather than a suffix.
	for m := 0; m < mssql.NumLockModes; m++ {
		add("read: lock mode "+mssql.LockHint(m),
			runtime.SpliceTree(selectPrefix+mssql.LockHint(m), eq, lw,
				limit+mssql.LockSuffix(m)).SQL, "a@b.c", 10)
	}

	add("count", runtime.SpliceTree(mssql.CountPrefix(table), eq, lw, "").SQL, "a@b.c")
	add("exists", runtime.SpliceTree(mssql.ExistsPrefix(table), eq, lw,
		mssql.ExistsSuffix()).SQL, "a@b.c")

	// --- the operators -------------------------------------------------------

	add("predicate: LIKE",
		runtime.SpliceTree(selectPrefix, []runtime.Tok{runtime.MakeLeaf(opLike, colEmail)},
			lw, limit).SQL, "a%", 10)
	add("predicate: IS NULL",
		runtime.SpliceTree(selectPrefix, []runtime.Tok{runtime.MakeLeaf(opIsNull, colDeleted)},
			lw, limit).SQL, 10)
	add("predicate: LOWER() equality",
		runtime.SpliceTree(selectPrefix, []runtime.Tok{runtime.MakeLeaf(opEqLower, colEmail)},
			lw, limit).SQL, "A@B.C", 10)
	add("predicate: AND of two",
		runtime.SpliceTree(selectPrefix, []runtime.Tok{
			runtime.MakeLeaf(opEq, colEmail), runtime.MakeLeaf(opGt, colRank),
			runtime.MakeGroup(runtime.KAnd, 2),
		}, lw, limit).SQL, "a@b.c", int64(1), 10)

	// The IN list: one bound document, so the statement's text does not depend
	// on how many values the caller passed (ADR-0010).
	inA, inB := mssql.InFrag(mssql.Ident("rank"), "BIGINT", false)
	add("predicate: IN via OPENJSON",
		selectPrefix+" WHERE "+strings.Replace(inA, mssql.Placeholder, "@p1", 1)+inB+
			" ORDER BY [rank] OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY",
		"[1,2,3]", 10)

	// Keyset pagination, which has no row constructor here and must expand.
	add("keyset: expanded row comparison",
		runtime.SpliceTree(selectPrefix, []runtime.Tok{
			runtime.MakeCol(colRank), runtime.MakeCol(colID),
			runtime.MakeRowCmp(0, 2),
		}, lw, limit).SQL, int64(1), "00000000-0000-0000-0000-000000000000", 10)

	// --- the semi-joins ------------------------------------------------------

	add("semi-join: EXISTS",
		mssql.SelectPrefix("ms_orgs", []string{"id", "name"})+" WHERE "+
			mssql.ExistsFrag(table, "org_id", "ms_orgs", "id", live)+
			" ORDER BY [id] OFFSET 0 ROWS FETCH NEXT @p1 ROWS ONLY", 10)
	add("semi-join: NOT EXISTS",
		mssql.SelectPrefix("ms_orgs", []string{"id", "name"})+" WHERE "+
			mssql.NotExistsFrag(table, "org_id", "ms_orgs", "id", live)+
			" ORDER BY [id] OFFSET 0 ROWS FETCH NEXT @p1 ROWS ONLY", 10)

	// --- greatest-n-per-group ------------------------------------------------

	order := []string{mssql.OrderTerm(1, mssql.Ident("rank"))}
	add("top-N: CROSS APPLY",
		runtime.SpliceOrder(
			mssql.TopNLateral(table, readCols, "org_id", "UNIQUEIDENTIFIER", live),
			order, mssql.OrderLead, mssql.OrderSep),
		jsonKeys(), int64(3))
	add("top-N: row_number window",
		runtime.SpliceOrder(
			mssql.TopNWindow(table, readCols, "org_id", "UNIQUEIDENTIFIER", live),
			order, mssql.OrderLead, mssql.OrderSep),
		jsonKeys(), int64(3))

	// --- recursion -----------------------------------------------------------

	for _, dir := range []struct {
		name string
		d    int
	}{{"descend", mssql.Descend}, {"ascend", mssql.Ascend}} {
		add("recursive: "+dir.name,
			mssql.Recursive("ms_orgs", []string{"id", "name", "parent_id"},
				"id", "parent_id", "UNIQUEIDENTIFIER", dir.d, ""),
			jsonKeys(), int64(5))
	}

	// --- the write path ------------------------------------------------------

	insCols := []string{"id", "email", "rank", "org_id"}
	quoted := make([]string, len(insCols))
	for i, c := range insCols {
		quoted[i] = mssql.Ident(c)
	}
	open_, sep, mid, close_ := mssql.InsertParts(readCols)
	insParts := runtime.InsertParts{Open: open_, Sep: sep, Mid: mid, Close: close_}
	add("insert: masked, OUTPUT before VALUES",
		runtime.SpliceInsertWith(mssql.InsertPrefix(table), insParts, quoted,
			runtime.MSSQLPlaceholder, "").SQL,
		newUUID(), "gate-insert@example.com", int64(7), orgID)

	full, err := mssql.InsertStmt(table, insCols, readCols)
	if err != nil {
		t.Fatal(err)
	}
	add("insert: full row, ordinals fixed at generate time",
		full, newUUID(), "gate-full@example.com", int64(8), orgID)

	setFrag := func(col string) runtime.Frag {
		a, b := mssql.SetFrag(col)
		return runtime.Frag{A: a, B: b}
	}
	whereFrag := func(col string) runtime.Frag {
		a, b, _ := mssql.Frag("Eq", mssql.Ident(col))
		return runtime.Frag{A: a, B: b}
	}
	updSecs := []runtime.Section{
		{Lead: mssql.SetLead, Sep: mssql.SetSep,
			Frags: []runtime.Frag{setFrag("email"), setFrag("rank")}},
		{Lead: mssql.WhereLead, Sep: mssql.WhereSep,
			Frags: []runtime.Frag{whereFrag("id")}},
	}
	add("update: OUTPUT between the assignments and the predicate",
		runtime.SpliceSectionsOutput(mssql.UpdatePrefix(table), updSecs,
			mssql.ReturningClause(readCols), "", runtime.MSSQLPlaceholder).SQL,
		"gate-updated@example.com", int64(9), memberID)

	delSecs := []runtime.Section{
		{Lead: mssql.WhereLead, Sep: mssql.WhereSep,
			Frags: []runtime.Frag{whereFrag("id")}},
	}
	add("delete", runtime.SpliceSectionsOutput(mssql.DeletePrefix(table), delSecs,
		"", "", runtime.MSSQLPlaceholder).SQL, newUUID())

	add("soft delete", mssql.SoftDeleteSet(table, "deleted_at")+
		" WHERE [id] = @p1", newUUID())
	add("restore", mssql.RestoreSet(table, "deleted_at")+
		" WHERE [id] = @p1", newUUID())

	bumpA, _ := mssql.BumpFrag("rank")
	add("optimistic bump", mssql.UpdatePrefix(table)+bumpA+" WHERE [id] = @p1", newUUID())
	nowA, _ := mssql.NowFrag("updated_at")
	add("server clock", mssql.UpdatePrefix(table)+nowA+" WHERE [id] = @p1", newUUID())

	// --- run them ------------------------------------------------------------

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run(t, db, c)
		})
	}
}

// run prepares and executes one statement. Both, for the reason in the file
// header: preparing types the parameters and parses the text, executing
// converts the values.
func run(t *testing.T, db *sql.DB, c lowered) {
	t.Helper()
	ctx := context.Background()
	st, err := db.PrepareContext(ctx, c.sql)
	if err != nil {
		t.Fatalf("PREPARE refused\n  %s\n%v", c.sql, err)
	}
	defer st.Close()
	rows, err := st.QueryContext(ctx, c.args...)
	if err != nil {
		t.Fatalf("EXECUTE refused\n  %s\n  args %v\n%v", c.sql, c.args, err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows failed\n  %s\n%v", c.sql, err)
	}
}
