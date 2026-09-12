package codegen

import (
	"github.com/gsoultan/storm/compile/mariadb"
	"github.com/gsoultan/storm/compile/mysql"
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

	Recursive func(table string, cols []string, key, parent, keyType string,
		dir int, live string) string

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

	InsertStmt      func(table string, cols, returning []string) (string, error)
	InsertPrefix    func(string) string
	InsertParts     func() (open, sep, mid, close string)
	ReturningClause func([]string) string
	UpdatePrefix    func(string) string
	DeletePrefix    func(string) string
	SetFrag         func(string) (string, string)
	BumpFrag        func(string) (string, string)

	SetLead, SetSep     string
	WhereLead, WhereSep string
	Placeholder         string

	// PlaceholderExpr is how the GENERATED code names its placeholder policy in
	// the runtime.Lowering it builds. Empty means the zero value, which is
	// PostgreSQL — so a PostgreSQL package does not mention it and stays
	// byte-identical.
	PlaceholderExpr string

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
		InsertParts:     pgsql.InsertParts,
		ReturningClause: pgsql.ReturningClause,
		UpdatePrefix:    pgsql.UpdatePrefix,
		DeletePrefix:    pgsql.DeletePrefix,
		SetFrag:         pgsql.SetFrag,
		BumpFrag:        pgsql.BumpFrag,
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
				a, b := mysql.InFrag(ident, c.Type.SQL(), op == "NotIn")
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
		AggregateSelect: mysql.AggregateSelect,
		AggregateSuffix: mysql.AggregateSuffix,
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
		InsertParts:     mysql.InsertParts,
		// MySQL 8 cannot return the row it wrote. An empty clause here is not a
		// lowering — InsertStmt refuses a non-empty returning list outright, so
		// this is only ever asked for the empty case.
		ReturningClause: func(cols []string) string { return "" },
		UpdatePrefix:    mysql.UpdatePrefix,
		DeletePrefix:    mysql.DeletePrefix,
		SetFrag:         mysql.SetFrag,
		BumpFrag:        mysql.BumpFrag,
		SetLead:         mysql.SetLead,
		SetSep:          mysql.SetSep,
		WhereLead:       mysql.WhereLead,
		WhereSep:        mysql.WhereSep,
		Placeholder:     mysql.Placeholder,
		PlaceholderExpr: "runtime.MySQLPlaceholder",
		Upsert:          nil, // ON DUPLICATE KEY UPDATE names no target; see the field's note
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
	if g.lw.PlaceholderExpr == "" {
		return "runtime.SpliceSections"
	}
	return "runtime.SpliceSectionsWith"
}

func (g *gen) spliceTail() string {
	if g.lw.PlaceholderExpr == "" {
		return `""`
	}
	return `"", ` + g.lw.PlaceholderExpr
}
