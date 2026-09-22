package runtime

import (
	"strings"
	"testing"
)

// Each target's carrier, rendered. The four spellings are the whole of
// ADR-0010's problem, and a carrier that rendered another target's would
// produce statements that bind nothing and fail at the first call.
func TestEachTargetsPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Placeholder
		want string
	}{
		// PostgreSQL's `$n` is the zero value, so an existing caller keeps the
		// back end it already had.
		{"postgres", Placeholder{}, "$1, $2, $3"},
		// MySQL binds by POSITION, so every marker is identical and reusing
		// one is not possible — which is why a declared parameter used in two
		// union branches is refused there.
		{"mysql", MySQLPlaceholder, "?, ?, ?"},
		// T-SQL binds by NAME, and an identifier may not start with a digit —
		// `@1` is a syntax error, which is what the Prefix is for.
		{"mssql", MSSQLPlaceholder, "@p1, @p2, @p3"},
		// Oracle binds by name too, and a NUMBER is a legal one, so no prefix
		// is needed. Reusing an ordinal binds once, as it does on SQL Server.
		{"oracle", OraclePlaceholder, ":1, :2, :3"},
	} {
		var b strings.Builder
		for i := 1; i <= 3; i++ {
			if i > 1 {
				b.WriteString(", ")
			}
			tc.p.write(&b, i)
		}
		if got := b.String(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
