package codegen

import (
	"github.com/gsoultan/storm/compile/mariadb"
	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/oracle"
	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// lowering is the seam's query side, dispatched at GENERATE time.
//
// compile/pgsql's own note said the Dialect interface "waits for M9, when there
// are two implementations to generalise from". There are two now, and this is
// the generalisation — deliberately a struct of function values rather than an
// interface, because the PostgreSQL side is then the pgsql functions
// THEMSELVES, assigned straight across. A wrapper could drift; an assignment
// cannot, and generated PostgreSQL output stays byte-identical by construction
// rather than by test. (The test exists anyway.)
//
// Fields a dialect cannot express are left nil, and the emitter that needs one
// refuses for that dialect naming the construct. That is the same rule the
// column types already follow: a construct the target cannot express is a
// generation error, never a silent omission.
type lowering struct {
	Ident func(string) string
	Frag  func(op, ident string, col *schema.Column) (a, b string, ok bool)

	SelectPrefix      func(table string, cols []string) string
	CountPrefix       func(string) string
	ExistsPrefix      func(string) string
	ExistsSuffix      func() string
	LimitOffsetSuffix func(withOffset bool) string
	DefaultOrderBy    func(pk []string, fallback string) string

	OrderTerm            func(dir int, ident string) string
	OrderLead, OrderSep  string
	NDirections          int
	TupleOpen, TupleSep  string
	TupleClose           string
	RowCmpOp             func(int) string
	NumLockModes         int
	LockSuffix, LockName func(int) string
	LockDoc              func(int) string
	LockNotes            func() []string
	LockRefusedCounted   func() string
	LockRefusedProbed    func() string
	LockRefusedGrouped   func() string
	LockRefusedJoined    func() string

	// TopN* take keyType for both dialects even though PostgreSQL's window
	// form does not need one: a uniform signature is what lets the emitter
	// stay dialect-blind, and the alternative is a branch in codegen, which
	// is the thing the seam exists to remove.
	// UnionSelect is fallible: a bare-placeholder back end cannot bind one
	// declared parameter to two branches, and an aggregate FILTER has no
	// MySQL form. Both change the RESULT rather than its spelling, so they
	// are refused at generate time rather than approximated.
	// Join* are fallible for the same reason Union* are: a back end may be
	// unable to express a declaration rather than merely spell it differently.
	JoinSelect func(table string, j *schema.Join,
		aggFor func(schema.CTE) (string, string), live func(table, alias string) string) (string, error)
	JoinSuffix        func(j *schema.Join) (string, error)
	JoinDeclaredWhere func(j *schema.Join, driving string) (string, error)

	UnionSelect func(u *schema.Union, live func(string) string) (string, error)
	UnionSuffix func(u *schema.Union) (string, error)

	// Recursive takes keyType for the same reason the batch loads do: the
	// roots arrive as a bound list, and MySQL has no array parameter to
	// compare one against.
	// Aggregate* are fallible: MySQL has only WITH ROLLUP, so an aggregation
	// over arbitrary grouping combinations has no lowering rather than a
	// different spelling.
	AggregateSelect func(table string, agg *schema.Aggregate) (string, error)
	AggregateSuffix func(agg *schema.Aggregate) (string, error)

	// KeyType spells a column's type for the places a lowering has to NAME one
	// — the JSON_TABLE COLUMNS declaration a bound key list is unpacked
	// through. codegen must not spell it itself: `uuid` is PostgreSQL's word
	// for BINARY(16), and passing it through emitted a MySQL statement that
	// PARSES nowhere. Every default storm model has a uuid key, so that was
	// every fetch plan.
	KeyType func(c *schema.Column) string

	Recursive func(table string, cols []string, key, parent, keyType string,
		dir int, live string) string

	// KeysAreClientSide says this target cannot express storm's uuid default,
	// so the generated write path has to fill the key itself.
	//
	// MySQL's UUID() is version 1 — it embeds the SERVER'S MAC ADDRESS in a
	// value that ends up in URLs, and it is a different version from the one
	// the model asked for — so borrowing it would be worse than having none.
	// Without this a caller who relied on the default got an all-zero primary
	// key, which makes the second insert a duplicate and the first row
	// unfindable by anything but a scan.
	KeysAreClientSide bool

	// RecursiveMaxDepth is the deepest traversal whose cycle guard still holds,
	// or 0 for no limit. A back end that accumulates visited keys in a
	// fixed-width column has one; PostgreSQL's array has none.
	RecursiveMaxDepth func(keyType string) int64

	TopNWindow  func(table string, cols []string, key, keyType, live string) string
	TopNLateral func(table string, cols []string, key, keyType, live string) string

	ExistsFrag    func(childTable, childFK, parentTable, parentPK, childLive string) string
	NotExistsFrag func(childTable, childFK, parentTable, parentPK, childLive string) string
	ExistsOpen    func(childTable, childFK, parentTable, parentPK, childLive string) string
	NotExistsOpen func(childTable, childFK, parentTable, parentPK, childLive string) string

	SoftDeleteWhere func(col string) string
	SoftDeleteSet   func(table, col string) string
	RestoreSet      func(table, col string) string
	LiveFor         func(alias, col string) string

	InsertStmt   func(table string, cols, returning []string) (string, error)
	InsertPrefix func(string) string
	// InsertParts takes the RETURNING list, because a back end whose returning
	// clause is POSITIONAL carries it in this punctuation rather than in a
	// suffix. SQL Server's OUTPUT goes between the column list and VALUES, and
	// at the end it is a syntax error.
	InsertParts     func(returning []string) (open, sep, mid, close string)
	ReturningClause func([]string) string
	UpdatePrefix    func(string) string
	DeletePrefix    func(string) string
	SetFrag         func(string) (string, string)
	BumpFrag        func(string) (string, string)
	NowFrag         func(string) (string, string)

	SetLead, SetSep     string
	WhereLead, WhereSep string
	Placeholder         string

	// PlaceholderExpr is how the GENERATED code names its placeholder policy in
	// the runtime.Lowering it builds. Empty means the zero value, which is
	// PostgreSQL — so a PostgreSQL package does not mention it and stays
	// byte-identical.
	PlaceholderExpr string

	// LockHint is the row lock written after the TABLE NAME rather than at the
	// end of the statement, or nil where the lock is a suffix.
	//
	// SQL Server has no FOR UPDATE: the equivalent is `WITH (UPDLOCK, ROWLOCK)`
	// attached to the table reference in FROM. Emitted only for a back end that
	// has one, so PostgreSQL's and MySQL's generated packages are unchanged.
	LockHint func(int) string

	// PagingOffsetFirst says the paging arguments bind offset before limit,
	// because the suffix names them in that order. `OFFSET m ROWS FETCH NEXT n
	// ROWS ONLY` is the reverse of `LIMIT n OFFSET m`, and the binder has to be
	// told rather than guess from the text.
	PagingOffsetFirst bool

	// ReturningPositional says the returning clause is not a trailing suffix,
	// so the write splicer has to be handed it separately.
	ReturningPositional bool

	// RowCmpExpand and OrderFallback are carried through to the generated
	// runtime.Lowering. Both are empty or false for the back ends that need
	// neither, so their output does not mention them.
	RowCmpExpand  bool
	OrderFallback string

	// upsertSkipped is why this back end generates no upsert, in the generated
	// package's own words. MySQL's conflict handling names no index; SQL
	// Server's MERGE names one but is a different STATEMENT rather than a
	// clause, so the two are skipped for different reasons and the comment
	// should say which.
	upsertSkipped string

	// Merge is the upsert for a back end whose conflict handling is a
	// STATEMENT rather than a clause on the insert, and nil for the others.
	//
	// SQL Server's MERGE is the one. Nothing about it can be appended to an
	// insert — it has a target, a source, a match condition and a branch per
	// outcome — so the whole shape comes from the back end and the generated
	// code splices it instead of the insert.
	Merge func(table string) mergeParts

	// MergeOutput is the clause a MERGE hands its row back through, which sits
	// in a third position again: after the branches and before the terminator.
	MergeOutput func([]string) string

	// Upsert is nil for a back end whose conflict handling is not the
	// inference form. MySQL's ON DUPLICATE KEY UPDATE names no target at all —
	// it fires on ANY unique key — so a generated OnConflictEmail() would be a
	// lie about which index it watched. Refused rather than approximated.
	Upsert *upsertLowering

	// name is what a refusal calls this dialect.
	name string

	// noReturning marks a back end that cannot hand back the row it wrote.
	// Insert then leaves the caller's Row untouched rather than racing a second
	// SELECT for the values — see compile/mysql.ErrNoReturning.
	noReturning bool
}

// canReturn reports whether an insert can learn what the server computed.
func (l lowering) canReturn() bool { return !l.noReturning }

// mergeParts is the punctuation a MERGE is spliced from. It mirrors
// runtime.MergeParts field for field, because it IS that — carried through
// codegen without codegen importing runtime or naming a keyword.
type mergeParts struct {
	Into, Sep, AsSrc, OnLead, OnSep, Eq, Tgt, Src string
	Matched, NotMatched, Values, Close, End       string
}

// upsertLowering is the conflict-target form, which only PostgreSQL has today.
type upsertLowering struct {
	Any, DoNothing, DoUpdate string
	Spec                     func(keys []pgsql.ConflictKey, where string) string
	ExcludedAssign           func(string) string
}

func loweringFor(d Dialect) lowering {
	switch d {
	case DialectMySQL:
		return mysqlLowering()
	case DialectMariaDB:
		return mariadbLowering()
	case DialectMSSQL:
		return mssqlLowering()
	case DialectOracle:
		return oracleLowering()
	}
	return postgresLowering()
}

// mariadbLowering is mysqlLowering with the five things that differ.
//
// Starting from MySQL's and overriding is the point of a struct of function
// values rather than an interface: MariaDB IS four fifths MySQL, and a second
// full implementation would be four fifths duplicate — which is where the drift
// would happen. What is listed here is exactly what diverges, measured against
// 11.4.13, and a reader can see the whole difference in one place.
func mariadbLowering() lowering {
	l := mysqlLowering()
	l.name = "mariadb"

	// The difference that pays for the dialect: MariaDB can return the row it
	// wrote, so Insert keeps the semantics MySQL cannot give it.
	l.InsertStmt = mariadb.InsertStmt
	l.ReturningClause = mariadb.ReturningClause
	l.noReturning = false

	// FOR SHARE does not exist; LOCK IN SHARE MODE does.
	l.LockSuffix = mariadb.LockSuffix

	// No LATERAL. The window form is not a fallback here, it is the only one.
	l.TopNLateral = func(t string, cols []string, key, keyType, live string) string {
		return mariadb.TopNBatch(t, cols, key, keyType, mysql.Live(live))
	}

	// WITH ROLLUP cannot be combined with ORDER BY, and storm always orders a
	// grouped read.
	l.AggregateSuffix = mariadb.AggregateSuffix

	return l
}

// postgresLowering assigns the pgsql functions across. Nothing is wrapped, so
// nothing can drift.
func postgresLowering() lowering {
	return lowering{
		name:  "postgres",
		Ident: pgsql.Ident,
		Frag: func(op, ident string, _ *schema.Column) (string, string, bool) {
			return pgsql.Frag(op, ident)
		},
		SelectPrefix:       pgsql.SelectPrefix,
		CountPrefix:        pgsql.CountPrefix,
		ExistsPrefix:       pgsql.ExistsPrefix,
		ExistsSuffix:       pgsql.ExistsSuffix,
		LimitOffsetSuffix:  pgsql.LimitOffsetSuffix,
		DefaultOrderBy:     pgsql.DefaultOrderBy,
		OrderTerm:          pgsql.OrderTerm,
		OrderLead:          pgsql.OrderLead,
		OrderSep:           pgsql.OrderSep,
		NDirections:        pgsql.NDirections,
		TupleOpen:          pgsql.TupleOpen,
		TupleSep:           pgsql.TupleSep,
		TupleClose:         pgsql.TupleClose,
		RowCmpOp:           pgsql.RowCmpOp,
		NumLockModes:       pgsql.NumLockModes,
		LockSuffix:         func(m int) string { return pgsql.LockSuffix(pgsql.LockMode(m)) },
		LockName:           func(m int) string { return pgsql.LockName(pgsql.LockMode(m)) },
		LockDoc:            func(m int) string { return pgsql.LockDoc(pgsql.LockMode(m)) },
		LockNotes:          pgsql.LockNotes,
		LockRefusedCounted: pgsql.LockRefusedCounted,
		LockRefusedProbed:  pgsql.LockRefusedProbed,
		LockRefusedGrouped: pgsql.LockRefusedGrouped,
		LockRefusedJoined:  pgsql.LockRefusedJoined,
		JoinSelect: func(t string, j *schema.Join, aggFor func(schema.CTE) (string, string),
			live func(table, alias string) string) (string, error) {
			return pgsql.JoinSelect(t, j, aggFor,
				func(tb, al string) pgsql.Live { return pgsql.Live(live(tb, al)) }), nil
		},
		JoinSuffix: func(j *schema.Join) (string, error) { return pgsql.JoinSuffix(j), nil },
		JoinDeclaredWhere: func(j *schema.Join, driving string) (string, error) {
			return pgsql.JoinDeclaredWhere(j, pgsql.Live(driving)), nil
		},
		UnionSelect: func(u *schema.Union, live func(string) string) (string, error) {
			return pgsql.UnionSelect(u, func(t string) pgsql.Live { return pgsql.Live(live(t)) }), nil
		},
		UnionSuffix: func(u *schema.Union) (string, error) { return pgsql.UnionSuffix(u), nil },
		AggregateSelect: func(t string, a *schema.Aggregate) (string, error) {
			return pgsql.AggregateSelect(t, a), nil
		},
		AggregateSuffix: func(a *schema.Aggregate) (string, error) {
			return pgsql.AggregateSuffix(a), nil
		},
		// PostgreSQL's own spelling, so the emitted text is unchanged.
		KeyType: func(c *schema.Column) string { return c.Type.SQL() },
		// An array has no declared width, so no depth is out of reach.
		RecursiveMaxDepth: func(string) int64 { return 0 },
		// gen_random_uuid() and uuidv7() are the server's job here.
		KeysAreClientSide: false,
		Recursive: func(t string, cols []string, key, parent, _ string, dir int, live string) string {
			return pgsql.Recursive(t, cols, key, parent, dir, pgsql.Live(live))
		},
		TopNWindow: func(t string, cols []string, key, _, live string) string {
			return pgsql.TopNWindow(t, cols, key, pgsql.Live(live))
		},
		TopNLateral: func(t string, cols []string, key, keyType, live string) string {
			return pgsql.TopNLateral(t, cols, key, keyType, pgsql.Live(live))
		},
		ExistsFrag: func(ct, fk, pt, pk, live string) string {
			return pgsql.ExistsFrag(ct, fk, pt, pk, pgsql.Live(live))
		},
		NotExistsFrag: func(ct, fk, pt, pk, live string) string {
			return pgsql.NotExistsFrag(ct, fk, pt, pk, pgsql.Live(live))
		},
		ExistsOpen: func(ct, fk, pt, pk, live string) string {
			return pgsql.ExistsOpen(ct, fk, pt, pk, pgsql.Live(live))
		},
		NotExistsOpen: func(ct, fk, pt, pk, live string) string {
			return pgsql.NotExistsOpen(ct, fk, pt, pk, pgsql.Live(live))
		},
		SoftDeleteWhere: pgsql.SoftDeleteWhere,
		SoftDeleteSet:   pgsql.SoftDeleteSet,
		RestoreSet:      pgsql.RestoreSet,
		LiveFor:         func(alias, col string) string { return string(pgsql.LiveFor(alias, col)) },
		InsertStmt: func(t string, c, r []string) (string, error) {
			return pgsql.InsertStmt(t, c, r), nil
		},
		InsertPrefix:    pgsql.InsertPrefix,
		InsertParts:     func([]string) (string, string, string, string) { return pgsql.InsertParts() },
		ReturningClause: pgsql.ReturningClause,
		UpdatePrefix:    pgsql.UpdatePrefix,
		DeletePrefix:    pgsql.DeletePrefix,
		SetFrag:         pgsql.SetFrag,
		BumpFrag:        pgsql.BumpFrag,
		NowFrag:         pgsql.NowFrag,
		SetLead:         pgsql.SetLead,
		SetSep:          pgsql.SetSep,
		WhereLead:       pgsql.WhereLead,
		WhereSep:        pgsql.WhereSep,
		Placeholder:     pgsql.Placeholder,
		Upsert: &upsertLowering{
			Any: pgsql.ConflictAny, DoNothing: pgsql.ConflictDoNothing,
			DoUpdate: pgsql.ConflictDoUpdate,
			Spec:     pgsql.ConflictSpec, ExcludedAssign: pgsql.ExcludedAssign,
		},
	}
}

func mysqlLowering() lowering {
	return lowering{
		name:  "mysql",
		Ident: mysql.Ident,
		// In and NotIn need the column's SQL type: the JSON_TABLE COLUMNS
		// declaration must match the column it is compared against, or the
		// comparison is a JSON scalar against a native value — wrong, and
		// unindexable. This is why Frag takes a column here and not just a
		// name.
		Frag: func(op, ident string, c *schema.Column) (string, string, bool) {
			switch op {
			case "In", "NotIn":
				if c == nil {
					return "", "", false
				}
				a, b := mysql.InFrag(ident, mysql.ColumnType(c), op == "NotIn")
				return a, b, true
			}
			return mysql.Frag(op, ident)
		},
		SelectPrefix:       mysql.SelectPrefix,
		CountPrefix:        mysql.CountPrefix,
		ExistsPrefix:       mysql.ExistsPrefix,
		ExistsSuffix:       mysql.ExistsSuffix,
		LimitOffsetSuffix:  mysql.LimitOffsetSuffix,
		DefaultOrderBy:     mysql.DefaultOrderBy,
		OrderTerm:          mysql.OrderTerm,
		OrderLead:          mysql.OrderLead,
		OrderSep:           mysql.OrderSep,
		NDirections:        mysql.NDirections,
		TupleOpen:          mysql.TupleOpen,
		TupleSep:           mysql.TupleSep,
		TupleClose:         mysql.TupleClose,
		RowCmpOp:           mysql.RowCmpOp,
		NumLockModes:       mysql.NumLockModes,
		LockSuffix:         mysql.LockSuffix,
		LockName:           func(m int) string { return pgsql.LockName(pgsql.LockMode(m)) },
		LockDoc:            func(m int) string { return pgsql.LockDoc(pgsql.LockMode(m)) },
		LockNotes:          pgsql.LockNotes,
		LockRefusedCounted: pgsql.LockRefusedCounted,
		LockRefusedProbed:  pgsql.LockRefusedProbed,
		LockRefusedGrouped: pgsql.LockRefusedGrouped,
		LockRefusedJoined:  pgsql.LockRefusedJoined,
		JoinSelect: func(t string, j *schema.Join, aggFor func(schema.CTE) (string, string),
			live func(table, alias string) string) (string, error) {
			return mysql.JoinSelect(t, j, aggFor,
				func(tb, al string) mysql.Live { return mysql.Live(live(tb, al)) })
		},
		JoinSuffix: mysql.JoinSuffix,
		JoinDeclaredWhere: func(j *schema.Join, driving string) (string, error) {
			return mysql.JoinDeclaredWhere(j, mysql.Live(driving))
		},
		UnionSelect: func(u *schema.Union, live func(string) string) (string, error) {
			return mysql.UnionSelect(u, func(t string) mysql.Live { return mysql.Live(live(t)) })
		},
		UnionSuffix: func(u *schema.Union) (string, error) {
			if err := mysql.UnionOrderRefused(u); err != nil {
				return "", err
			}
			return mysql.UnionSuffix(u), nil
		},
		AggregateSelect:   mysql.AggregateSelect,
		AggregateSuffix:   mysql.AggregateSuffix,
		KeyType:           mysql.ColumnType,
		RecursiveMaxDepth: mysql.MaxRecursionDepth,
		KeysAreClientSide: true,
		Recursive: func(t string, cols []string, key, parent, keyType string, dir int, live string) string {
			return mysql.Recursive(t, cols, key, parent, keyType, dir, mysql.Live(live))
		},
		TopNWindow: func(t string, cols []string, key, keyType, live string) string {
			return mysql.TopNWindow(t, cols, key, keyType, mysql.Live(live))
		},
		TopNLateral: func(t string, cols []string, key, keyType, live string) string {
			return mysql.TopNLateral(t, cols, key, keyType, mysql.Live(live))
		},
		ExistsFrag: func(ct, fk, pt, pk, live string) string {
			return mysql.ExistsFrag(ct, fk, pt, pk, mysql.Live(live))
		},
		NotExistsFrag: func(ct, fk, pt, pk, live string) string {
			return mysql.NotExistsFrag(ct, fk, pt, pk, mysql.Live(live))
		},
		ExistsOpen: func(ct, fk, pt, pk, live string) string {
			return mysql.ExistsOpen(ct, fk, pt, pk, mysql.Live(live))
		},
		NotExistsOpen: func(ct, fk, pt, pk, live string) string {
			return mysql.NotExistsOpen(ct, fk, pt, pk, mysql.Live(live))
		},
		SoftDeleteWhere: mysql.SoftDeleteWhere,
		SoftDeleteSet:   mysql.SoftDeleteSet,
		RestoreSet:      mysql.RestoreSet,
		LiveFor:         mysql.LiveFor,
		InsertStmt:      mysql.InsertStmt,
		InsertPrefix:    mysql.InsertPrefix,
		InsertParts:     func([]string) (string, string, string, string) { return mysql.InsertParts() },
		// MySQL 8 cannot return the row it wrote. An empty clause here is not a
		// lowering — InsertStmt refuses a non-empty returning list outright, so
		// this is only ever asked for the empty case.
		ReturningClause: func(cols []string) string { return "" },
		UpdatePrefix:    mysql.UpdatePrefix,
		DeletePrefix:    mysql.DeletePrefix,
		SetFrag:         mysql.SetFrag,
		BumpFrag:        mysql.BumpFrag,
		NowFrag:         mysql.NowFrag,
		SetLead:         mysql.SetLead,
		SetSep:          mysql.SetSep,
		WhereLead:       mysql.WhereLead,
		WhereSep:        mysql.WhereSep,
		Placeholder:     mysql.Placeholder,
		PlaceholderExpr: "runtime.MySQLPlaceholder",
		Upsert:          nil, // ON DUPLICATE KEY UPDATE names no target; see the field's note
		upsertSkipped:   "its conflict handling names no index, so a method named after one would watch something else",
		noReturning:     true,
		// Standard SQL in shape, but every one of them renders identifiers and
		// placeholders through compile/pgsql today. Refused rather than
		// silently emitted in the other dialect's spelling; lowering them is
		// what remains of M9's query side.
	}
}

// spliceFn and spliceTail name the write splice a generated package calls.
//
// PostgreSQL keeps the three-argument runtime.SpliceSections it always used, so
// its output is byte-identical to what it emitted before the placeholder
// carrier existed — an adopter regenerating on a storm that gained MySQL
// support should see no diff at all. Only a back end that needs a different
// placeholder names the four-argument form.
func (g *gen) spliceFn() string {
	if g.lw.ReturningPositional {
		// The clause is not a suffix on this target: it sits between the
		// assignments and the predicate, and at the end it is a syntax error.
		return "runtime.SpliceSectionsOutput"
	}
	if g.lw.PlaceholderExpr == "" {
		return "runtime.SpliceSections"
	}
	return "runtime.SpliceSectionsWith"
}

func (g *gen) spliceTail() string {
	if g.lw.ReturningPositional {
		// out, suffix, placeholder. A statement with nothing to hand back
		// passes an empty clause rather than a different function, so the two
		// shapes cannot drift.
		return `"", "", ` + g.lw.PlaceholderExpr
	}
	if g.lw.PlaceholderExpr == "" {
		return `""`
	}
	return `"", ` + g.lw.PlaceholderExpr
}

// spliceTailWith is spliceTail for a statement that DOES hand something back,
// naming the expression that holds the clause.
func (g *gen) spliceTailWith(clause string) string {
	if g.lw.ReturningPositional {
		return clause + `, "", ` + g.lw.PlaceholderExpr
	}
	if g.lw.PlaceholderExpr == "" {
		return clause
	}
	return clause + ", " + g.lw.PlaceholderExpr
}

// mssqlLowering assigns the compile/mssql functions across.
//
// A THIRD implementation, and the first that is not a variation on either of
// the others. What it needed from the seam that M9 did not is the four fields
// above — a lock that is a table hint, paging operands in the other order, a
// returning clause that is positional, and a row comparison with no
// constructor to write. Each is a construct SQL Server has no other spelling
// for, so each is the difference between a generated package that runs and one
// that is a syntax error.
func mssqlLowering() lowering {
	return lowering{
		name:  "mssql",
		Ident: mssql.Ident,
		Frag: func(op, ident string, c *schema.Column) (string, string, bool) {
			switch op {
			case "In", "NotIn":
				// The OPENJSON column must be declared with the SAME type as
				// the column it is matched against, or the comparison puts an
				// implicit conversion on the indexed side and the seek becomes
				// a scan. This is why Frag takes a column here and not a name.
				if c == nil {
					return "", "", false
				}
				a, b := mssql.InFrag(ident, mssql.ColumnType(c), op == "NotIn")
				return a, b, true
			}
			return mssql.Frag(op, ident)
		},
		SelectPrefix:      mssql.SelectPrefix,
		CountPrefix:       mssql.CountPrefix,
		ExistsPrefix:      mssql.ExistsPrefix,
		ExistsSuffix:      mssql.ExistsSuffix,
		LimitOffsetSuffix: mssql.LimitOffsetSuffix,
		DefaultOrderBy:    mssql.DefaultOrderBy,
		OrderTerm:         mssql.OrderTerm,
		OrderLead:         mssql.OrderLead,
		OrderSep:          mssql.OrderSep,
		NDirections:       mssql.NDirections,
		TupleOpen:         mssql.TupleOpen,
		TupleSep:          mssql.TupleSep,
		TupleClose:        mssql.TupleClose,
		RowCmpOp:          mssql.RowCmpOp,
		RowCmpExpand:      mssql.RowCmpExpand,
		OrderFallback:     mssql.OrderFallback,
		PagingOffsetFirst: mssql.PagingOffsetFirst,

		NumLockModes: mssql.NumLockModes,
		LockSuffix:   mssql.LockSuffix,
		LockHint:     mssql.LockHint,
		LockName:     pgsqlLockName,
		LockDoc:      pgsqlLockDoc,
		LockNotes:    mssql.LockNotes,

		LockRefusedCounted: pgsql.LockRefusedCounted,
		LockRefusedProbed:  pgsql.LockRefusedProbed,
		LockRefusedGrouped: pgsql.LockRefusedGrouped,
		LockRefusedJoined:  pgsql.LockRefusedJoined,

		JoinSelect: func(t string, j *schema.Join, aggFor func(schema.CTE) (string, string),
			live func(table, alias string) string) (string, error) {
			return mssql.JoinSelect(t, j, aggFor,
				func(tb, al string) mssql.Live { return mssql.Live(live(tb, al)) })
		},
		JoinSuffix: mssql.JoinSuffix,
		JoinDeclaredWhere: func(j *schema.Join, driving string) (string, error) {
			return mssql.JoinDeclaredWhere(j, mssql.Live(driving))
		},
		UnionSelect: func(u *schema.Union, live func(string) string) (string, error) {
			return mssql.UnionSelect(u, func(t string) mssql.Live { return mssql.Live(live(t)) })
		},
		UnionSuffix: func(u *schema.Union) (string, error) {
			if err := mssql.UnionOrderRefused(u); err != nil {
				return "", err
			}
			return mssql.UnionSuffix(u), nil
		},
		AggregateSelect: mssql.AggregateSelect,
		AggregateSuffix: mssql.AggregateSuffix,

		KeyType:           mssql.ColumnType,
		RecursiveMaxDepth: mssql.MaxRecursionDepth,
		// NEWID() is a server-side version 4 uuid, so unlike MySQL the key
		// stays the database's job here: with a DEFAULT and an OUTPUT clause,
		// an insert that names no key comes back carrying one.
		KeysAreClientSide: false,

		Recursive: func(t string, cols []string, key, parent, keyType string, dir int, live string) string {
			return mssql.Recursive(t, cols, key, parent, keyType, dir, mssql.Live(live))
		},
		TopNWindow: func(t string, cols []string, key, keyType, live string) string {
			return mssql.TopNWindow(t, cols, key, keyType, mssql.Live(live))
		},
		TopNLateral: func(t string, cols []string, key, keyType, live string) string {
			return mssql.TopNLateral(t, cols, key, keyType, mssql.Live(live))
		},
		ExistsFrag: func(ct, fk, pt, pk, live string) string {
			return mssql.ExistsFrag(ct, fk, pt, pk, mssql.Live(live))
		},
		NotExistsFrag: func(ct, fk, pt, pk, live string) string {
			return mssql.NotExistsFrag(ct, fk, pt, pk, mssql.Live(live))
		},
		ExistsOpen: func(ct, fk, pt, pk, live string) string {
			return mssql.ExistsOpen(ct, fk, pt, pk, mssql.Live(live))
		},
		NotExistsOpen: func(ct, fk, pt, pk, live string) string {
			return mssql.NotExistsOpen(ct, fk, pt, pk, mssql.Live(live))
		},

		SoftDeleteWhere: mssql.SoftDeleteWhere,
		SoftDeleteSet:   mssql.SoftDeleteSet,
		RestoreSet:      mssql.RestoreSet,
		LiveFor:         mssql.LiveFor,

		InsertStmt:          mssql.InsertStmt,
		InsertPrefix:        mssql.InsertPrefix,
		InsertParts:         mssql.InsertParts,
		ReturningClause:     mssql.ReturningClause,
		ReturningPositional: mssql.ReturningPositional,
		UpdatePrefix:        mssql.UpdatePrefix,
		DeletePrefix:        mssql.DeletePrefix,
		SetFrag:             mssql.SetFrag,
		BumpFrag:            mssql.BumpFrag,
		NowFrag:             mssql.NowFrag,
		SetLead:             mssql.SetLead,
		SetSep:              mssql.SetSep,
		WhereLead:           mssql.WhereLead,
		WhereSep:            mssql.WhereSep,
		Placeholder:         mssql.Placeholder,
		PlaceholderExpr:     "runtime.MSSQLPlaceholder",

		// Not the inference form: MERGE is a statement of its own. See Merge.
		Upsert: nil,
		Merge: func(table string) mergeParts {
			p := mssql.Merge(table)
			return mergeParts{
				Into: p.Into, Sep: p.Sep, AsSrc: p.AsSrc, OnLead: p.OnLead,
				OnSep: p.OnSep, Eq: p.Eq, Tgt: p.Tgt, Src: p.Src,
				Matched: p.Matched, NotMatched: p.NotMatched,
				Values: p.Values, Close: p.Close, End: p.End,
			}
		},
		MergeOutput: mssql.MergeOutput,
	}
}

// oracleLowering is the FOURTH back end, and the first whose generated package
// reads DECODED values rather than wire bytes.
//
// Most of the grammar is SQL Server's — OFFSET/FETCH, CROSS APPLY, no FILTER
// clause — and three things diverge in ways worth naming here rather than only
// in compile/oracle:
//
//   - It cannot RETURN the row it wrote. Oracle's RETURNING binds OUTPUT
//     parameters and runtime.Executor carries none, so keys are client-side —
//     MySQL's answer from a different direction.
//   - Three of the seven lock modes are refused: there is no shared ROW lock.
//     And a locked read may not be capped, which is the work-queue shape.
//   - The row constructor WORKS, so keyset pagination needs no expansion. That
//     is the one place this target is closer to PostgreSQL than to SQL Server,
//     and it was measured rather than read.
func oracleLowering() lowering {
	return lowering{
		name:  "oracle",
		Ident: oracle.Ident,
		Frag: func(op, ident string, c *schema.Column) (string, string, bool) {
			switch op {
			case "In", "NotIn":
				// The JSON_TABLE column must be declared with the SAME type as
				// the column it is matched against, or the comparison puts an
				// implicit conversion on the indexed side and the seek becomes
				// a scan. This is why Frag takes a column here and not a name.
				if c == nil {
					return "", "", false
				}
				a, b := oracle.InFrag(ident, oracle.ColumnType(c), op == "NotIn")
				return a, b, true
			}
			return oracle.Frag(op, ident)
		},
		SelectPrefix:      oracle.SelectPrefix,
		CountPrefix:       oracle.CountPrefix,
		ExistsPrefix:      oracle.ExistsPrefix,
		ExistsSuffix:      oracle.ExistsSuffix,
		LimitOffsetSuffix: oracle.LimitOffsetSuffix,
		DefaultOrderBy:    oracle.DefaultOrderBy,
		OrderTerm:         oracle.OrderTerm,
		OrderLead:         oracle.OrderLead,
		OrderSep:          oracle.OrderSep,
		NDirections:       oracle.NDirections,
		TupleOpen:         oracle.TupleOpen,
		TupleSep:          oracle.TupleSep,
		TupleClose:        oracle.TupleClose,
		RowCmpOp:          oracle.RowCmpOp,
		RowCmpExpand:      oracle.RowCmpExpand,
		OrderFallback:     oracle.OrderFallback,
		PagingOffsetFirst: oracle.PagingOffsetFirst,

		NumLockModes: oracle.NumLockModes,
		LockSuffix:   oracle.LockSuffix,
		LockHint:     oracle.LockHint,
		LockName:     pgsqlLockName,
		LockDoc:      pgsqlLockDoc,
		LockNotes:    oracle.LockNotes,

		LockRefusedCounted: pgsql.LockRefusedCounted,
		LockRefusedProbed:  pgsql.LockRefusedProbed,
		LockRefusedGrouped: pgsql.LockRefusedGrouped,
		LockRefusedJoined:  pgsql.LockRefusedJoined,

		JoinSelect: func(t string, j *schema.Join, aggFor func(schema.CTE) (string, string),
			live func(table, alias string) string) (string, error) {
			return oracle.JoinSelect(t, j, aggFor,
				func(tb, al string) oracle.Live { return oracle.Live(live(tb, al)) })
		},
		JoinSuffix: oracle.JoinSuffix,
		JoinDeclaredWhere: func(j *schema.Join, driving string) (string, error) {
			return oracle.JoinDeclaredWhere(j, oracle.Live(driving))
		},
		UnionSelect: func(u *schema.Union, live func(string) string) (string, error) {
			return oracle.UnionSelect(u, func(t string) oracle.Live { return oracle.Live(live(t)) })
		},
		UnionSuffix: func(u *schema.Union) (string, error) {
			// UnionOrderRefused is always nil here — Oracle has NULLS
			// FIRST/LAST wherever an ORDER BY appears — and is still called,
			// because a back end answering "nothing" is not the same as one
			// the seam does not ask.
			if err := oracle.UnionOrderRefused(u); err != nil {
				return "", err
			}
			return oracle.UnionSuffix(u), nil
		},
		AggregateSelect: oracle.AggregateSelect,
		AggregateSuffix: oracle.AggregateSuffix,

		KeyType:           oracle.ColumnType,
		RecursiveMaxDepth: oracle.MaxRecursionDepth,
		// The key is storm's to generate: Oracle's RETURNING binds output
		// parameters the port does not carry, and SYS_GUID() is not a uuid at
		// all. Same answer MySQL gives, reached from a different direction.
		KeysAreClientSide: true,

		Recursive: func(t string, cols []string, key, parent, keyType string, dir int, live string) string {
			return oracle.Recursive(t, cols, key, parent, keyType, dir, oracle.Live(live))
		},
		TopNWindow: func(t string, cols []string, key, keyType, live string) string {
			return oracle.TopNWindow(t, cols, key, keyType, oracle.Live(live))
		},
		TopNLateral: func(t string, cols []string, key, keyType, live string) string {
			return oracle.TopNLateral(t, cols, key, keyType, oracle.Live(live))
		},
		ExistsFrag: func(ct, fk, pt, pk, live string) string {
			return oracle.ExistsFrag(ct, fk, pt, pk, oracle.Live(live))
		},
		NotExistsFrag: func(ct, fk, pt, pk, live string) string {
			return oracle.NotExistsFrag(ct, fk, pt, pk, oracle.Live(live))
		},
		ExistsOpen: func(ct, fk, pt, pk, live string) string {
			return oracle.ExistsOpen(ct, fk, pt, pk, oracle.Live(live))
		},
		NotExistsOpen: func(ct, fk, pt, pk, live string) string {
			return oracle.NotExistsOpen(ct, fk, pt, pk, oracle.Live(live))
		},

		SoftDeleteWhere: oracle.SoftDeleteWhere,
		SoftDeleteSet:   oracle.SoftDeleteSet,
		RestoreSet:      oracle.RestoreSet,
		LiveFor:         oracle.LiveFor,

		InsertStmt:      oracle.InsertStmt,
		InsertPrefix:    oracle.InsertPrefix,
		InsertParts:     func([]string) (string, string, string, string) { return oracle.InsertParts() },
		ReturningClause: oracle.ReturningClause,
		UpdatePrefix:    oracle.UpdatePrefix,
		DeletePrefix:    oracle.DeletePrefix,
		SetFrag:         oracle.SetFrag,
		BumpFrag:        oracle.BumpFrag,
		NowFrag:         oracle.NowFrag,
		SetLead:         oracle.SetLead,
		SetSep:          oracle.SetSep,
		WhereLead:       oracle.WhereLead,
		WhereSep:        oracle.WhereSep,
		Placeholder:     oracle.Placeholder,
		PlaceholderExpr: "runtime.OraclePlaceholder",

		// No upsert lowering yet. Oracle HAS MERGE — it was measured in
		// internal/oraclespike before this package was written — and the
		// generated form needs a returning clause to hand the row back, which
		// this target does not have. Both are left out together rather than
		// shipping an upsert that silently returns nothing.
		Upsert: nil,

		// A generated package for this target cannot return the row it wrote.
		// Oracle's RETURNING binds OUTPUT parameters and runtime.Executor
		// carries none, so keys are client-side. See compile/oracle/write.go.
		noReturning: true,
	}
}

// The lock names and documentation are pgsql's, because the MODE numbering is
// the generated Query's and not a back end's to choose — a mode is an index
// into a statement-cache array, and renaming it per dialect would make the
// same method mean different things.
func pgsqlLockName(m int) string { return pgsql.LockName(pgsql.LockMode(m)) }
func pgsqlLockDoc(m int) string  { return pgsql.LockDoc(pgsql.LockMode(m)) }
