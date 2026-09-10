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

// Only the key's type moves between two loads of different key types — the
// shape is otherwise fixed at generate time.
func TestBatchLoadShapeDependsOnlyOnTheKeyType(t *testing.T) {
	a := mysql.TopNLateral("t", []string{"id"}, "fk", "BIGINT", "")
	b := mysql.TopNLateral("t", []string{"id"}, "fk", "BINARY(16)", "")
	if strings.Replace(b, "BINARY(16)", "BIGINT", 1) != a {
		t.Errorf("the two differ by more than the key type:\n%s\n%s", a, b)
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
