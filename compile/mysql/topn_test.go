package mysql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/pgsql"
)

// The construct M9's exit gate names, and where the dialects genuinely part
// company: PostgreSQL binds a whole parent-key list as ONE array and unnests
// it; MySQL has neither an array type nor unnest.

func TestBatchLoadBindsOneJSONDocumentNotAList(t *testing.T) {
	for name, sql := range map[string]string{
		"lateral": mysql.TopNLateral("members", []string{"id"}, "org_id", "BIGINT", ""),
		"window":  mysql.TopNWindow("members", []string{"id"}, "org_id", "BIGINT", ""),
	} {
		// Two placeholders and no more: the key list and N. An IN (?,?,?) form
		// would bind one per key, making the statement shape a function of the
		// caller's data rather than of the program.
		if n := strings.Count(sql, "?"); n != 2 {
			t.Errorf("%s binds %d placeholders, want 2 (the key list, then N):\n%s", name, n, sql)
		}
		if !strings.Contains(sql, "JSON_TABLE") {
			t.Errorf("%s does not use the JSON_TABLE lowering ADR-0010 chose:\n%s", name, sql)
		}
		// The COLUMNS declaration must be TYPED to the key it joins against.
		// Declared JSON it compares a JSON scalar to a native value: wrong, and
		// unindexable — and losing the index is the whole reason this form was
		// chosen over reading every child of every parent.
		if !strings.Contains(sql, "`_storm_k` BIGINT PATH '$'") {
			t.Errorf("%s does not type the unpacked key to the column:\n%s", name, sql)
		}
		if strings.Contains(sql, "unnest") {
			t.Errorf("%s uses unnest, which MySQL has not:\n%s", name, sql)
		}
	}
}

// Only the key's TYPE moves between two loads whose keys are both JSON-native
// — the shape is otherwise fixed at generate time.
func TestBatchLoadShapeDependsOnlyOnTheKeyType(t *testing.T) {
	a := mysql.TopNLateral("t", []string{"id"}, "fk", "BIGINT", "")
	b := mysql.TopNLateral("t", []string{"id"}, "fk", "INT", "")
	if strings.Replace(b, "INT", "BIGINT", 1) != a {
		t.Errorf("the two differ by more than the key type:\n%s\n%s", a, b)
	}
}

// A BINARY key cannot travel in a JSON document as itself: JSON is text, and
// arbitrary bytes are not valid UTF-8. It goes as hex and comes back through
// UNHEX.
//
// This is the DEFAULT key, not an edge case — storm.Model gives every table a
// BINARY(16) uuid — and before this the generator emitted the storm type name
// `uuid` into the COLUMNS clause, which MySQL cannot parse. Every fetch plan on
// a default model was a syntax error.
func TestABinaryKeyTravelsAsHex(t *testing.T) {
	for name, sql := range map[string]string{
		"lateral": mysql.TopNLateral("members", []string{"id"}, "org_id", "BINARY(16)", ""),
		"window":  mysql.TopNWindow("members", []string{"id"}, "org_id", "BINARY(16)", ""),
		"in":      inFragSQL("BINARY(16)"),
	} {
		if strings.Contains(sql, "BINARY(16) PATH") {
			t.Errorf("%s declares a binary JSON_TABLE column, which cannot hold bytes:\n%s",
				name, sql)
		}
		// Two hex characters per byte, or the value is silently truncated.
		if !strings.Contains(sql, "CHAR(32) PATH '$'") {
			t.Errorf("%s does not unpack the key as 32 hex characters:\n%s", name, sql)
		}
		if !strings.Contains(sql, "UNHEX(") {
			t.Errorf("%s never decodes the hex back to the column's type:\n%s", name, sql)
		}
		// UNHEX is applied to the JSON side, never to the indexed column:
		// comparing HEX(id) would read the same rows and lose the index, which
		// is the whole reason this form was chosen.
		if strings.Contains(sql, "HEX(`org_id`)") || strings.Contains(sql, "HEX(`id`)") {
			t.Errorf("%s wraps the INDEXED column rather than the JSON value:\n%s", name, sql)
		}
	}
}

func inFragSQL(colType string) string {
	a, b := mysql.InFrag("`org_id`", colType, false)
	return a + b
}

// A key that IS representable in JSON must not be wrapped in anything.
func TestANonBinaryKeyIsNotDecoded(t *testing.T) {
	sql := mysql.TopNLateral("members", []string{"id"}, "org_id", "BIGINT", "")
	if strings.Contains(sql, "UNHEX") {
		t.Errorf("a BIGINT key was decoded as though it were hex:\n%s", sql)
	}
}

// A marked row must not occupy one of the N slots, or a parent whose most
// recent children are all deleted gets an empty page instead of its live ones.
func TestBatchLoadExcludesDeletedBeforeTheLimit(t *testing.T) {
	live := mysql.Live(mysql.SoftDeleteWhere("deleted_at"))
	for name, sql := range map[string]string{
		"lateral": mysql.TopNLateral("t", []string{"id"}, "fk", "BIGINT", live),
		"window":  mysql.TopNWindow("t", []string{"id"}, "fk", "BIGINT", live),
	} {
		if !strings.Contains(sql, "deleted_at") {
			t.Errorf("%s carries no soft-delete predicate:\n%s", name, sql)
		}
		if i, j := strings.Index(sql, "deleted_at"), strings.LastIndex(sql, "LIMIT"); j >= 0 && i > j {
			t.Errorf("%s filters after the limit:\n%s", name, sql)
		}
	}
}

// PostgreSQL is untouched: it still binds an array, which is the cheaper form
// where it exists.
func TestPostgresStillUnnestsAnArray(t *testing.T) {
	sql := pgsql.TopNLateral("t", []string{"id"}, "fk", "uuid", "")
	if !strings.Contains(sql, "unnest") {
		t.Errorf("the PostgreSQL batch loader stopped unnesting:\n%s", sql)
	}
}
