package storm

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// validateAggregate rejects an aggregation PostgreSQL would refuse.
//
// In a grouped query every column reference has to be either one of the
// grouping expressions or inside an aggregate — otherwise there is no single
// value for it in the group. PostgreSQL says so at execution:
//
//	column "orders.placed_at" must appear in the GROUP BY clause
//	or be used in an aggregate function
//
// which is a correct message arriving at the worst time, from a report that may
// only run at month end. The window clauses are where this is easiest to get
// wrong: `row_number() OVER (ORDER BY placed_at)` next to
// `GROUP BY date_trunc('day', placed_at)` looks reasonable and is not.
func validateAggregate(tbl *schema.Table, agg *schema.Aggregate) error {
	for _, p := range agg.Params {
		if p.Type.Name == "" {
			return fmt.Errorf(
				"%s: aggregate %q declares parameter %q and never compares it with "+
					"a column — it would sit in the generated signature demanding an "+
					"argument that reaches no statement",
				tbl.Name, agg.Name, p.Name)
		}
	}
	if len(agg.By) == 0 {
		// No GROUP BY: the whole table is one group and every output must be
		// an aggregate. A bare column here is the same error by another route.
		for _, t := range agg.Terms {
			if col, bad := ungrouped(t.Expr, nil); bad {
				return fmt.Errorf(
					"%s: aggregate %q selects column %s but groups by nothing — "+
						"with no grouping the whole table is one group, so every output "+
						"must be an aggregate",
					tbl.Name, agg.Name, col)
			}
		}
		// The same rule reaches HAVING: with no GROUP BY there is still no
		// single value of a bare column for the one group.
		if agg.Having != nil {
			if col, bad := ungroupedCond(*agg.Having, nil); bad {
				return fmt.Errorf(
					"%s: aggregate %q has a HAVING on column %s but groups by nothing — "+
						"wrap it in an aggregate, or reference a declared output with storm.Out(...)",
					tbl.Name, agg.Name, col)
			}
		}
		return nil
	}

	grouped := make([]schema.Expr, 0, len(agg.By))
	for _, g := range agg.By {
		grouped = append(grouped, g.Expr)
	}
	for _, t := range agg.Terms {
		// Named before the general rule, because the general message sends the
		// reader to "group by it or wrap it in an aggregate" and neither is the
		// fix. The fix is the nested form, which SumOver and its siblings build.
		if col, bad := windowedOverRow(t.Expr, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q windows %s(%s) in %q, but the query is grouped — "+
					"with OVER it is a window function, so its argument is read from the "+
					"GROUPED rows and column %s is not there\n"+
					"       to aggregate ACROSS the groups use %sOver(handle, %q, window), "+
					"which builds %s(%s(...)) OVER (...)",
				tbl.Name, agg.Name, t.Expr.Fn, col, t.As, col,
				exportedFn(t.Expr.Fn), t.As, t.Expr.Fn, t.Expr.Fn)
		}
		if col, bad := ungrouped(t.Expr, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q reads column %s in %q, but groups by something else — "+
					"a column in a grouped query must be one of the grouping expressions "+
					"or inside an aggregate\n"+
					"       group by it, wrap it in an aggregate, or reference a declared "+
					"output with storm.Out(...)",
				tbl.Name, agg.Name, col, t.As)
		}
	}
	if agg.Having != nil {
		if col, bad := ungroupedCond(*agg.Having, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q has a HAVING on column %s, which is neither grouped "+
					"nor aggregated", tbl.Name, agg.Name, col)
		}
	}
	return nil
}

// ungrouped walks an expression and returns the first column reference that is
// neither one of the grouping expressions nor inside an aggregate.
//
// An aggregate's ARGUMENTS and its FILTER are exempt: both are evaluated per
// row within the group. Its OVER clause is not — a window over grouped rows
// sees one row per group.
func ungrouped(e schema.Expr, grouped []schema.Expr) (string, bool) {
	// Checked WHOLE first, before descending. PostgreSQL allows any expression
	// that appears in the GROUP BY, not just bare columns — so
	// date_trunc('day', placed_at) is fine even though placed_at alone is not,
	// and descending into its arguments first would report the opposite.
	for _, g := range grouped {
		if exprEqual(e, g) {
			return "", false
		}
	}
	switch e.Kind {
	case schema.ExprCol:
		return e.Col, true

	case schema.ExprAgg:
		// An aggregate's arguments AND its FILTER are evaluated per row inside
		// the group, so both may read any column — `count(*) FILTER (WHERE
		// total >= 50)` alongside `GROUP BY status` is valid, and asserted
		// against a live server by the example's TestAggregateFilter.
		//
		// UNLESS it has an OVER clause. Then it is a window function call, not
		// an aggregate: its arguments are evaluated over the query's OUTPUT
		// rows, which in a grouped query are groups. `sum(total) OVER (...)`
		// alongside `GROUP BY status` reads a column that no longer exists per
		// output row, and PostgreSQL says so — "column must appear in the GROUP
		// BY clause or be used in an aggregate function". The form that means
		// "across the groups" is sum(sum(total)) OVER (...), which is what
		// SumOver and its siblings build.
		if e.Over != nil {
			for _, a := range e.Args {
				if col, bad := ungrouped(a, grouped); bad {
					return col, true
				}
			}
		}
		// The OVER clause itself is checked the same way: a window over grouped
		// rows sees one row per group, so it may only name grouping
		// expressions or aggregates.
		return ungroupedWindow(e.Over, grouped)

	case schema.ExprGrouping:
		// GROUPING() names grouping columns by definition.
		return "", false

	case schema.ExprWindow:
		for _, a := range e.Args {
			if col, bad := ungrouped(a, grouped); bad {
				return col, true
			}
		}
		return ungroupedWindow(e.Over, grouped)

	default:
		for _, a := range e.Args {
			if col, bad := ungrouped(a, grouped); bad {
				return col, true
			}
		}
	}
	return "", false
}

func ungroupedWindow(w *schema.Window, grouped []schema.Expr) (string, bool) {
	if w == nil {
		return "", false
	}
	for _, p := range w.PartitionBy {
		if col, bad := ungrouped(p, grouped); bad {
			return col, true
		}
	}
	for _, o := range w.OrderBy {
		if col, bad := ungrouped(o.Expr, grouped); bad {
			return col, true
		}
	}
	return "", false
}

func ungroupedCond(c schema.Cond, grouped []schema.Expr) (string, bool) {
	switch c.Kind {
	case schema.CondCmp:
		if col, bad := ungrouped(c.Left, grouped); bad {
			return col, true
		}
		return ungrouped(c.Right, grouped)
	case schema.CondIsNull, schema.CondIsNotNull:
		return ungrouped(c.Left, grouped)
	default:
		for _, a := range c.Args {
			if col, bad := ungroupedCond(a, grouped); bad {
				return col, true
			}
		}
	}
	return "", false
}

// exprEqual is structural equality, which is how PostgreSQL decides whether an
// expression "appears in the GROUP BY". date_trunc('day', placed_at) in the
// SELECT matches date_trunc('day', placed_at) in the GROUP BY; a different unit
// does not.
func exprEqual(a, b schema.Expr) bool {
	if a.Kind != b.Kind || a.Col != b.Col || a.Fn != b.Fn || a.Arith != b.Arith ||
		len(a.Args) != len(b.Args) {
		return false
	}
	if a.Kind == schema.ExprLit && a.Lit != b.Lit {
		return false
	}
	for i := range a.Args {
		if !exprEqual(a.Args[i], b.Args[i]) {
			return false
		}
	}
	return true
}

// windowedOverRow reports an aggregate that carries a window AND reads a column
// straight from the table. It is the one shape whose generic diagnosis points
// the wrong way, so it is diagnosed on its own.
func windowedOverRow(e schema.Expr, grouped []schema.Expr) (string, bool) {
	if e.Kind != schema.ExprAgg || e.Over == nil {
		return "", false
	}
	for _, a := range e.Args {
		if col, bad := ungrouped(a, grouped); bad {
			return col, true
		}
	}
	return "", false
}

// exportedFn is the builder method name for an aggregate: sum → Sum.
func exportedFn(fn string) string {
	if fn == "" {
		return fn
	}
	return strings.ToUpper(fn[:1]) + fn[1:]
}

// Index validation — what a declaration can get wrong that the database would
// report only at apply time, or worse, accept and quietly normalise away so
// that the next diff proposes the same index again.
//
// The second kind is the one worth a build-time rule. PostgreSQL prints only
// the non-default NULL placement back, so an ascending key declared NULLS LAST
// is stored, read back without the flag, compared against the model, and
// dropped and recreated on every run of `storm diff`. That is not a wrong
// index; it is a migration that never converges, which is worse.

// indexMethods are the access methods storm can name, per the target that
// has them.
var indexMethods = map[string]string{
	BTree: "PostgreSQL and MySQL", Hash: "PostgreSQL and MySQL",
	GIN: "PostgreSQL", GiST: "PostgreSQL", SPGiST: "PostgreSQL", BRIN: "PostgreSQL",
	FullText: "MySQL",
}

// opClassMethods lists, for the operator classes storm knows, the methods
// that provide them. An unknown class is passed through for the server to
// judge; a known one on the wrong method is refused here, where the message
// can say which method it wanted.
var opClassMethods = map[string][]string{
	"text_pattern_ops":    {BTree},
	"varchar_pattern_ops": {BTree},
	"bpchar_pattern_ops":  {BTree},
	"jsonb_ops":           {GIN},
	"jsonb_path_ops":      {GIN},
	"array_ops":           {GIN},
	"gin_trgm_ops":        {GIN},
	"gist_trgm_ops":       {GiST},
	"tsvector_ops":        {GIN, GiST},
	"inet_ops":            {GiST, SPGiST},
	"range_ops":           {GiST, SPGiST},
	"point_ops":           {GiST, SPGiST},
	"box_ops":             {GiST},
}

// storageParams lists, per parameter, the methods that accept it.
var storageParams = map[string][]string{
	"fillfactor":             {BTree, Hash, GiST, SPGiST},
	"deduplicate_items":      {BTree},
	"fastupdate":             {GIN},
	"gin_pending_list_limit": {GIN},
	"pages_per_range":        {BRIN},
	"autosummarize":          {BRIN},
	"buffering":              {GiST},
}

// includeMethods are the methods whose leaf entries can carry INCLUDE columns.
var includeMethods = map[string]bool{BTree: true, GiST: true, SPGiST: true}

func (b *builder) validateIndexes() {
	for _, mi := range b.ordered {
		t := mi.tbl.out
		byName := map[string]int{}
		for _, ix := range t.Indexes {
			byName[t.IndexName(ix)]++
		}
		for _, ix := range t.Indexes {
			b.validateIndex(t, ix, byName)
		}
	}
}

func (b *builder) validateIndex(t *schema.Table, ix *schema.Index, byName map[string]int) {
	name := t.IndexName(ix)
	fail := func(format string, a ...any) {
		b.errs.add(fmt.Errorf("%s: index %s "+format, append([]any{t.Name, name}, a...)...))
	}
	if byName[name] > 1 {
		// Two indexes over the same columns — one btree and one hash, or two
		// operator classes — generate the same name and the second CREATE
		// fails on the first's. The definition is legitimate; the name is not.
		fail("would be created twice — two indexes over the same columns need Named(...)")
		return
	}

	method := ix.Method
	if method == "" {
		method = BTree
	}
	if _, ok := indexMethods[method]; !ok {
		known := make([]string, 0, len(indexMethods))
		for m := range indexMethods {
			known = append(known, m)
		}
		sortStrings(known)
		fail("uses access method %q, which storm does not know — one of %s",
			method, strings.Join(known, ", "))
		return
	}
	if ix.Unique && method != BTree {
		fail("is UNIQUE and %s, and only a btree can enforce uniqueness", method)
	}
	if ix.NullsNotDistinct && !ix.Unique {
		fail("is NULLS NOT DISTINCT but not UNIQUE — the clause only means something for a uniqueness check")
	}
	if method == Hash && len(ix.Columns) > 1 {
		fail("is a hash over %d columns; a hash index has exactly one key", len(ix.Columns))
	}
	if len(ix.Include) > 0 && !includeMethods[method] {
		fail("carries INCLUDE columns on a %s index; only btree, gist and spgist leaf entries can carry them", method)
	}
	for _, p := range ix.With {
		methods, ok := storageParams[p.Name]
		if !ok {
			known := make([]string, 0, len(storageParams))
			for k := range storageParams {
				known = append(known, k)
			}
			sortStrings(known)
			fail("sets storage parameter %q, which storm does not know — one of %s", p.Name, strings.Join(known, ", "))
			continue
		}
		if !containsStr(methods, method) {
			fail("sets %s, a %s parameter, on a %s index", p.Name, strings.Join(methods, "/"), method)
		}
	}

	keys := map[string]bool{}
	for _, c := range ix.Columns {
		if !c.Expr {
			if keys[c.Name] {
				fail("names column %s twice", c.Name)
			}
			keys[c.Name] = true
		}
		b.validateKey(t, ix, method, c, fail)
	}
	for _, inc := range ix.Include {
		if keys[inc] {
			fail("has %s as both a key and an INCLUDE column; a key is already in every entry", inc)
		}
	}
	if method == FullText {
		for _, c := range ix.Columns {
			if c.Expr || !textual(t.Column(c.Name)) {
				fail("is FULLTEXT over %s, which is not a text column", c.Name)
			}
		}
	}
}

func (b *builder) validateKey(t *schema.Table, ix *schema.Index, method string,
	c schema.IndexColumn, fail func(string, ...any)) {
	switch {
	case c.NullsFirst && c.NullsLast:
		fail("orders %s NULLS FIRST and NULLS LAST", c.Name)
	case c.Desc && c.NullsFirst:
		fail("orders %s DESC NULLS FIRST, which is where a descending key already puts NULLs — "+
			"the database will not remember the clause, and every diff would recreate the index", c.Name)
	case !c.Desc && c.NullsLast:
		fail("orders %s ASC NULLS LAST, which is where an ascending key already puts NULLs — "+
			"the database will not remember the clause, and every diff would recreate the index", c.Name)
	}
	if c.OpClass != "" {
		if methods, known := opClassMethods[c.OpClass]; known && !containsStr(methods, method) {
			fail("gives %s the operator class %s, which belongs to %s, on a %s index — Using(storm.%s)",
				c.Name, c.OpClass, strings.Join(methods, "/"), method, constName(methods[0]))
		}
	}
	if c.Prefix > 0 {
		if c.Expr {
			fail("indexes a %d-character prefix of an expression; a prefix applies to a column", c.Prefix)
		} else if !textual(t.Column(c.Name)) && !binary(t.Column(c.Name)) {
			fail("indexes a %d-character prefix of %s, which is not a text or bytes column", c.Prefix, c.Name)
		}
	}
	_ = ix
}

func textual(c *schema.Column) bool {
	if c == nil {
		return false
	}
	return c.Type.Name == schema.TypeText || c.Type.Name == schema.TypeVarchar
}

func binary(c *schema.Column) bool {
	return c != nil && c.Type.Name == schema.TypeBytea
}

// constName is the Go constant for a method, for an error's fix.
func constName(method string) string {
	switch method {
	case BTree:
		return "BTree"
	case GIN:
		return "GIN"
	case GiST:
		return "GiST"
	case SPGiST:
		return "SPGiST"
	case Hash:
		return "Hash"
	case BRIN:
		return "BRIN"
	case FullText:
		return "FullText"
	}
	return method
}

func containsStr(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// Shard-key validation — what a sharding declaration can get wrong that no
// database will ever report.
//
// This pass is different in kind from the index and aggregate ones beside it.
// Those refuse things the server would reject, earlier and with a better
// message; the server is still a backstop. Here there is no backstop at all.
// Each shard holds an ordinary table with ordinary constraints, and nothing
// in any of them records that three other servers hold the rest of the rows.
// A query sent to the wrong shard does not fail — it returns fewer rows, and
// every constraint it touched was satisfied. So the only place a sharding
// mistake can be caught is here, before the code that would make it exists.

// shardKeyTypes are the column types a shard key may have: the three shapes
// shard.Key holds. A timestamp is absent because sharding by time puts every
// write on one shard, and a bool because two shards is not sharding.
var shardKeyTypes = map[string]bool{
	schema.TypeUUID:    true,
	schema.TypeText:    true,
	schema.TypeInt2:    true,
	schema.TypeInt4:    true,
	schema.TypeInt8:    true,
	schema.TypeVarchar: true,
}

func (b *builder) validateShardKeys() {
	sharded := map[string]string{} // table name -> shard key column
	for _, mi := range b.ordered {
		if t := mi.tbl.out; t.Sharded() {
			sharded[t.Name] = t.ShardKey
		}
	}
	if len(sharded) == 0 {
		return
	}
	byName := map[string]*schema.Table{}
	for _, t := range b.outSch.Tables {
		byName[t.Name] = t
	}
	for _, mi := range b.ordered {
		t := mi.tbl.out
		b.validateShardKey(t, sharded)
		b.validateShardedJoins(t, sharded)
		b.validateShardedLinks(t, sharded, byName)
	}
	b.validateShardedUnions(sharded)
}

// validateShardedLinks refuses an IMPLICIT many-to-many that touches a sharded
// table.
//
// The join table storm synthesizes for `Books []Book` holds exactly two
// columns — author_id and book_id — and there is no third one to put a shard
// key in. So it is not sharded, its generated package takes a
// runtime.Executor, and a link row can be written to any database in the set
// while both of the rows it joins live on one. That is the feature's own
// failure mode, on the table whose entire job is to join two sharded tables.
//
// storm will not invent the column. A shard key is a statement about where a
// row lives and which value decides it; synthesizing one would mean choosing a
// value at insert time from a row the adopter never declared. The way out is
// to declare the join table as a model — `t.Through` — which gives it columns,
// a shard key, and the ordinary checks every other table gets.
func (b *builder) validateShardedLinks(t *schema.Table, sharded map[string]string, byName map[string]*schema.Table) {
	for _, r := range t.Relations {
		if r.Link == "" {
			continue
		}
		link := byName[r.Link]
		if link == nil || !link.Generated {
			// Declared by the adopter: it is an ordinary table and the rules
			// above already judged it.
			continue
		}
		_, targetSharded := sharded[r.Target]
		if !t.Sharded() && !targetSharded {
			continue
		}
		which := t.Name
		key := t.ShardKey
		if !t.Sharded() {
			which, key = r.Target, sharded[r.Target]
		}
		b.errs.add(fmt.Errorf("%s: %s is a many-to-many through %s, which storm generated, and "+
			"%s is sharded by %s — the generated join table holds only the two foreign keys, so "+
			"it has no column to carry a shard key and its rows could be written to any database "+
			"in the set while the rows they join live on one. Declare the join table as a model "+
			"with t.Through, give it the shard key column, and storm will check it like any other "+
			"table",
			t.Name, r.Field, r.Link, which, key))
	}
}

// validateShardedJoins applies the relation rules to a DECLARED join, which
// reaches tables the relation graph does not: a join names its own tables and
// its CTEs name more.
//
// The declaring table is the FROM and is not in Tables, so it is the one whose
// shard the whole statement runs on.
func (b *builder) validateShardedJoins(t *schema.Table, sharded map[string]string) {
	for _, j := range t.Joins {
		reached := make([]string, 0, len(j.Tables)+len(j.CTEs))
		for _, jt := range j.Tables {
			if jt.Table != "" { // empty means the alias names a CTE
				reached = append(reached, jt.Table)
			}
		}
		for _, c := range j.CTEs {
			reached = append(reached, c.Table)
		}

		for _, name := range reached {
			key, isSharded := sharded[name]
			switch {
			case !t.Sharded() && isSharded:
				b.errs.add(fmt.Errorf("%s: join %s reads %s, which is sharded by %s, and %s is "+
					"not sharded — the join runs on whichever database the caller passed, so it "+
					"would see one shard's %s rows and call them all of them",
					t.Name, j.Name, name, key, t.Name, name))

			case t.Sharded() && isSharded && key != t.ShardKey:
				b.errs.add(fmt.Errorf("%s: join %s is sharded by %s and reads %s, which is "+
					"sharded by %s — a join is one statement on one database, and these two "+
					"tables' rows are not on the same one. Shard both by the same column",
					t.Name, j.Name, t.ShardKey, name, key))
			}
		}
	}
}

// validateShardedUnions refuses a union that reads a sharded table.
//
// A union has no driving table (ADR-0008), so its generated reader takes a
// runtime.Executor and there is no Bound for it to take instead — there is no
// single shard for a statement whose branches are separate tables. Even when
// every branch is sharded on the same column, the union is ONE statement and
// would have to run on one database while claiming to answer for all of them.
//
// Refused rather than quietly given an Executor, because a union over a
// sharded table returns rows: one shard's, presented as the whole answer.
func (b *builder) validateShardedUnions(sharded map[string]string) {
	for _, u := range b.outSch.Unions {
		for _, br := range u.Branches {
			key, isSharded := sharded[br.Table]
			if !isSharded {
				continue
			}
			b.errs.add(fmt.Errorf("union %s reads %s, which is sharded by %s — a union has no "+
				"driving table, so there is no shard to run it on and its reader would answer "+
				"for one database as if it were all of them. Read each shard with "+
				"shard.Set.Each and combine the results where you can say what combining means",
				u.Name, br.Table, key))
		}
	}
}

func (b *builder) validateShardKey(t *schema.Table, sharded map[string]string) {
	fail := func(format string, a ...any) {
		b.errs.add(fmt.Errorf("%s: "+format, append([]any{t.Name}, a...)...))
	}

	// The column, checked against the FINAL schema rather than against what it
	// looked like when ShardKey was called. `t.ShardKey(&o.TenantID)` followed
	// by `t.Col(&o.TenantID).Null()` in the same Schema method is a model that
	// passed the check at the call and is wrong by the end of it.
	if t.Sharded() {
		c := t.Column(t.ShardKey)
		switch {
		case c == nil:
			fail("the shard key names column %q, which this table does not have", t.ShardKey)
		case !c.NotNull:
			fail("%s is the shard key and is nullable — NULL names no shard, so a row with one "+
				"could not be written anywhere or found again", c.Name)
		case !shardKeyTypes[c.Type.Name]:
			fail("%s is the shard key and is %s — a shard key is a uuid, text or an integer, "+
				"because those are the shapes shard.Key holds", c.Name, c.Type.Name)
		}
	}

	for _, r := range t.Relations {
		targetKey, targetSharded := sharded[r.Target]

		switch {
		case !t.Sharded() && targetSharded:
			// The unsharded side's generated calls take a runtime.Executor,
			// which names no shard — so there is nothing to route the child
			// read with, and it would go to whichever database the caller
			// happened to hold. Unlike the reverse direction below, there is
			// no deployment in which this is correct.
			fail("%s points at %s, which is sharded by %s, and %s is not sharded — a read from "+
				"here carries no shard, so the %s rows would be fetched from whichever database "+
				"the caller passed. Shard %s by the same key, or drop the relation and look the "+
				"rows up through the shard set",
				r.Field, r.Target, targetKey, t.Name, r.Target, t.Name)

		case t.Sharded() && targetSharded && targetKey != t.ShardKey:
			// Two shard keys on one join is two answers for where the joined
			// row lives, and the query can only send one of them.
			fail("%s is sharded by %s and points at %s, which is sharded by %s — the two rows "+
				"can land on different databases and the read would find only the ones that did "+
				"not. Shard both by the same column, or denormalise %s onto %s",
				t.Name, t.ShardKey, r.Target, targetKey, targetKey, t.Name)
		}

		// t sharded, target NOT sharded is allowed, and is the reference-table
		// case: `countries`, `plans`, `currencies` — small, rarely written,
		// and copied to every shard. The child read runs against the parent's
		// own Bound, so it reads the copy on that shard, which is right IF the
		// copy is there. storm cannot check that a table exists on every shard
		// from a model file, so this is the one part of the arrangement the
		// adopter owns; `storm verify` run against each shard is what proves
		// it.
	}
}
