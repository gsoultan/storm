package oracle

import (
	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/schema"
)

// ColumnType is the Oracle type of a storm column, for the places a lowering
// has to NAME a type rather than just quote an identifier — the JSON_TABLE
// COLUMNS declaration a bound key list is unpacked through, and a CAST.
//
// It delegates to compile/oraddl rather than restating the map. Two maps for
// one question drift, and the direction they drift in is a key the DDL declared
// RAW(16) and the loader unpacks as text — which reads the right rows through
// an implicit conversion that makes the index unusable, or none at all.
//
// A type oraddl refuses has no Oracle spelling, so there is nothing to return.
// That cannot reach a generated package: the generator runs oraddl.Check first
// and refuses the model. The fallback exists so this function is total.
func ColumnType(c *schema.Column) string {
	if c.Type.Enum {
		// An enum column's values are its labels, and a JSON document carries
		// them as text. VARCHAR2(4000) rather than the exact label width,
		// because the document does not know the column's width and the
		// comparison is against it.
		return "VARCHAR2(4000)"
	}
	if t, err := oraddl.TypeSQL("", c); err == nil {
		// A CLOB cannot be a JSON_TABLE column type or an IN-list key: Oracle
		// refuses LOB comparison in most predicates. The unpack is text, and a
		// text column compared to VARCHAR2 is the conversion Oracle does on
		// the LITERAL side, which leaves an index usable.
		if t == "CLOB" {
			return "VARCHAR2(4000)"
		}
		return t
	}
	return "VARCHAR2(4000)"
}

// MaxRecursionDepth is the deepest traversal whose cycle guard still holds.
//
// Zero: there is none. Oracle's recursive WITH accumulates the visited path in
// whatever the anchor's expression types it as, and storm builds that path as
// VARCHAR2(4000) — which CAN overflow, unlike PostgreSQL's array and SQL
// Server's NVARCHAR(MAX).
//
// It is still zero rather than a number, because Oracle's overflow is an ERROR
// (ORA-01489, "result of string concatenation is too long") and not a silent
// truncation. That is the distinction that mattered on MySQL: its CHAR(4000)
// truncated quietly and a cycle guard comparing truncated paths stops guarding.
// An error is safe in the way silence is not.
func MaxRecursionDepth(string) int64 { return 0 }
