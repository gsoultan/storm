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
func (g *gen) live() pgsql.Live { return pgsql.Live(g.lw.LiveFor("", g.t.SoftDelete)) }

// liveIn looks a table up in the whole schema, for a read that spans more than
// one — a union branch, a joined table. Returns "" for a table that deletes
// rows for real, and for a table this schema does not contain.
func liveIn(lw lowering, s *schema.Schema, table, alias string) pgsql.Live {
	if s == nil {
		return ""
	}
	if t := s.Table(table); t != nil {
		return pgsql.Live(lw.LiveFor(alias, t.SoftDelete))
	}
	return ""
}

// joinLive is what JoinSelect takes: a table and the alias it is joined under.
func joinLive(lw lowering, s *schema.Schema) func(table, alias string) pgsql.Live {
	return func(table, alias string) pgsql.Live { return liveIn(lw, s, table, alias) }
}

// liveLookup is what the multi-table read builders take, so they can ask per
// table without knowing anything about storm's model layer.
func liveLookup(lw lowering, s *schema.Schema) func(string) pgsql.Live {
	return func(table string) pgsql.Live { return liveIn(lw, s, table, "") }
}

func (g *gen) splice(prefix, rest string) string {
	if !g.t.SoftDeletes() {
		return fmt.Sprintf("runtime.SpliceTree(%s, %s)", prefix, rest)
	}
	return fmt.Sprintf("runtime.SpliceTreeWhere(%s, softDeleteWhere, %s)", prefix, rest)
}

// widens says the model let call sites read this table's marked rows.
func (g *gen) widens() bool { return g.t.SoftDeletes() && g.t.ReadsDeleted }

// readStmts emits the statement pickers All, Count and Exists go through:
// the live rows, or — on a soft-delete table whose query asked WithDeleted —
// every row. The second is the one read that splices without the predicate,
// on purpose, and only a query that says so reaches it.
func (g *gen) readStmts() {
	if !g.widens() {
		g.p("func (q Query) readStmt(toks []runtime.Tok) *runtime.Stmt {")
		g.p("\treturn stmtFor(toks, q.offset > 0, q.lock)")
		g.p("}")
		g.p("")
		g.p("func (q Query) countStmt(toks []runtime.Tok) *runtime.Stmt  { return countStmtFor(toks) }")
		g.p("func (q Query) existsStmt(toks []runtime.Tok) *runtime.Stmt { return existsStmtFor(toks) }")
		g.p("")
		return
	}
	g.p("// WithDeleted reads the marked rows as well as the live ones. All, One,")
	g.p("// Count, Exists, Take and the projections honour it; every other terminal refuses it,")
	g.p("// rather than quietly answering about the live rows only.")
	g.p("func (q Query) WithDeleted() Query { q.deleted = true; return q }")
	g.p("")
	g.p("var (")
	g.p("\tdeletedReads = func() [%d][2]*runtime.TreeCache {", g.lw.NumLockModes)
	g.p("\t\tvar cs [%d][2]*runtime.TreeCache", g.lw.NumLockModes)
	g.p("\t\tfor i := range cs {")
	g.p("\t\t\tcs[i][0], cs[i][1] = runtime.NewTreeCache(), runtime.NewTreeCache()")
	g.p("\t\t}")
	g.p("\t\treturn cs")
	g.p("\t}()")
	g.p("\tdeletedCount  = runtime.NewTreeCache()")
	g.p("\tdeletedExists = runtime.NewTreeCache()")
	g.p(")")
	g.p("")
	g.p("func (q Query) readStmt(toks []runtime.Tok) *runtime.Stmt {")
	g.p("\tif !q.deleted {")
	g.p("\t\treturn stmtFor(toks, q.offset > 0, q.lock)")
	g.p("\t}")
	g.p("\ti, suffix := 0, limitSuffix")
	g.p("\tif q.offset > 0 {")
	g.p("\t\ti, suffix = 1, limitOffsetSuffix")
	g.p("\t}")
	g.p("\tc := deletedReads[q.lock][i]")
	g.p("\tif st := c.Get(toks); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	prefix := "selectPrefix"
	if g.lw.LockHint != nil {
		prefix = "selectPrefix + lockHint[q.lock]"
	}
	g.p("\treturn c.Put(toks, runtime.SpliceTree(%s, toks, lowering, suffix+lockSuffix[q.lock]))", prefix)
	g.p("}")
	g.p("")
	g.p("func (q Query) countStmt(toks []runtime.Tok) *runtime.Stmt {")
	g.p("\tif !q.deleted {")
	g.p("\t\treturn countStmtFor(toks)")
	g.p("\t}")
	g.p("\tif st := deletedCount.Get(toks); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\treturn deletedCount.Put(toks, runtime.SpliceTree(countPrefix, toks, lowering, \"\"))")
	g.p("}")
	g.p("")
	g.p("func (q Query) existsStmt(toks []runtime.Tok) *runtime.Stmt {")
	g.p("\tif !q.deleted {")
	g.p("\t\treturn existsStmtFor(toks)")
	g.p("\t}")
	g.p("\tif st := deletedExists.Get(toks); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\treturn deletedExists.Put(toks, runtime.SpliceTree(existsPrefix, toks, lowering, existsSuffix))")
	g.p("}")
	g.p("")
}
