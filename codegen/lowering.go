package codegen

import (
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
	if d == DialectMySQL {
		return mysqlLowering()
	}
	return postgresLowering()
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
		SoftDeleteWhere:    pgsql.SoftDeleteWhere,
		SoftDeleteSet:      pgsql.SoftDeleteSet,
		RestoreSet:         pgsql.RestoreSet,
		LiveFor:            func(alias, col string) string { return string(pgsql.LiveFor(alias, col)) },
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
		SoftDeleteWhere:    mysql.SoftDeleteWhere,
		SoftDeleteSet:      mysql.SoftDeleteSet,
		RestoreSet:         mysql.RestoreSet,
		LiveFor:            mysql.LiveFor,
		InsertStmt:         mysql.InsertStmt,
		InsertPrefix:       mysql.InsertPrefix,
		InsertParts:        mysql.InsertParts,
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
		Upsert:          nil, // ON DUPLICATE KEY UPDATE names no target; see the field's note
		noReturning:     true,
	}
}
