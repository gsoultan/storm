package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// splice picks the runtime splice for a read: the plain one, or the form that
// ANDs a declared predicate in front of the caller's.
//
// Every read of a soft-delete table goes through here. That is the point — the
// hazard docs/CONCEPT.md names is "every query that FORGETS the predicate", and
// the way to make forgetting impossible is to have exactly one place that
// decides, rather than a rule each emitter has to remember. A new read path
// that calls runtime.SpliceTree directly is the bug this function exists to
// prevent, and TestNoUnguardedReadOnSoftDeleteTable is what notices.
// live is this table's soft-delete predicate, unqualified — the form a
// single-table read wants. A read that names the table under an alias builds
// its own with pgsql.LiveFor.
func (g *gen) live() pgsql.Live { return pgsql.LiveFor("", g.t.SoftDelete) }

// liveIn looks a table up in the whole schema, for a read that spans more than
// one — a union branch, a joined table. Returns "" for a table that deletes
// rows for real, and for a table this schema does not contain.
func liveIn(s *schema.Schema, table, alias string) pgsql.Live {
	if s == nil {
		return ""
	}
	if t := s.Table(table); t != nil {
		return pgsql.LiveFor(alias, t.SoftDelete)
	}
	return ""
}

// joinLive is what JoinSelect takes: a table and the alias it is joined under.
func joinLive(s *schema.Schema) func(table, alias string) pgsql.Live {
	return func(table, alias string) pgsql.Live { return liveIn(s, table, alias) }
}

// liveLookup is what the multi-table read builders take, so they can ask per
// table without knowing anything about storm's model layer.
func liveLookup(s *schema.Schema) func(string) pgsql.Live {
	return func(table string) pgsql.Live { return liveIn(s, table, "") }
}

func (g *gen) splice(prefix, rest string) string {
	if !g.t.SoftDeletes() {
		return fmt.Sprintf("runtime.SpliceTree(%s, %s)", prefix, rest)
	}
	return fmt.Sprintf("runtime.SpliceTreeWhere(%s, softDeleteWhere, %s)", prefix, rest)
}
