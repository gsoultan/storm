package mssql

import (
	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/schema"
)

// ColumnType is the SQL Server type of a storm column, for the places a
// lowering has to NAME a type rather than just quote an identifier — the
// OPENJSON WITH declaration a bound key list is unpacked through, and a CAST.
//
// It delegates to compile/msddl rather than restating the map. Two maps for one
// question drift, and the direction they drift in is a key the DDL declared
// UNIQUEIDENTIFIER and the loader unpacks as text — which reads the right rows
// through an implicit conversion that makes the index unusable, or none at all.
//
// A type msddl refuses has no SQL Server spelling, so there is nothing to
// return. That cannot reach a generated package: the generator runs msddl.Check
// first and refuses the model. The fallback exists so this function is total.
func ColumnType(c *schema.Column) string {
	if c.Type.Enum {
		// An enum column's values are its labels, and a JSON document carries
		// them as text. NVARCHAR(4000) rather than the exact label width,
		// because OPENJSON's own default is 4000 and the comparison is against
		// a column whose width the document does not know.
		return "NVARCHAR(4000)"
	}
	if t, err := msddl.TypeSQL("", c); err == nil {
		return t
	}
	return "NVARCHAR(4000)"
}

// MaxRecursionDepth is the deepest traversal whose cycle guard still holds.
//
// Zero: there is none. SQL Server's recursive CTE accumulates the visited path
// in whatever the anchor's expression types it as, and storm builds that path
// as NVARCHAR(MAX) — which has no declared width to overflow, the same property
// PostgreSQL's array has and the property MySQL's CHAR(4000) lacks.
//
// The depth that DOES apply here is the server's own: a recursive CTE stops at
// 100 levels unless the statement says `OPTION (MAXRECURSION n)`. That is a
// refusal rather than a truncation — the server raises an error — so it is
// safe in the way MySQL's silent width overflow was not, and it is spelled on
// the statement by compile/mssql.Recursive.
func MaxRecursionDepth(string) int64 { return 0 }
