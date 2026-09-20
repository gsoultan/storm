// Package migrate diffs two schemas and emits a reviewable migration.
//
// The default path applies nothing (ADR-0001): Diff writes a numbered,
// forward-only file for your migration runner, and marks every step that could
// lose data so a destructive change cannot arrive unannounced.
//
// Auto is the exception, added by ADR-0001's 2026-09-07 amendment — automigrate,
// for the databases whose cost of being wrong is low. It applies the same plan
// directly, under a lock, in one transaction, and refuses to lose data unless
// told to. See auto.go.
package migrate

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Change is one migration step.
type Change struct {
	SQL string
	// Destructive means the step can lose data or break a running deployment:
	// dropping an object, narrowing a type, or adding a NOT NULL column with no
	// default to a table that may already have rows.
	Destructive bool
	// Why explains a destructive step, and is emitted as a comment.
	Why string
	// NoTransaction means the statement cannot run inside a transaction
	// block — CREATE INDEX CONCURRENTLY — and so cannot share a migration
	// file with anything else under a runner that wraps each file in one.
	// The tool writes each such change to a file of its own, and SQL marks it.
	NoTransaction bool

	// What the change creates or drops, when it is an index: enough to render
	// it again in the concurrent form once the plan knows the table is live.
	table     *schema.Table
	index     *schema.Index
	dropIndex string

	// addsEnumValue marks ALTER TYPE ... ADD VALUE. PostgreSQL will run it
	// inside a transaction but refuses to let anything USE the new label until
	// that transaction commits (SQLSTATE 55P04, "unsafe use of new value"), so
	// a step that adds a label and a step that gives a column a default of it
	// cannot share one. Auto commits these first, on their own; anything
	// applying a Plan as a single transaction has to do the same.
	addsEnumValue bool
}

// NoTransactionMarker is the comment SQL puts above a change that must run
// outside a transaction block, for a migration runner to act on.
const NoTransactionMarker = "-- storm:no-transaction"

// Plan is an ordered set of changes.
type Plan struct {
	Changes []Change

	// dialect is what the changes are written IN, remembered so Concurrently
	// can render them again. The zero value is PostgreSQL, which keeps a
	// hand-built Plan{Changes: ...} — tool does that to render a subset —
	// meaning what it always did.
	dialect Dialect
}

// Empty reports whether the two schemas already agree.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Destructive reports whether any step needs --allow-destructive.
func (p Plan) Destructive() bool {
	for _, c := range p.Changes {
		if c.Destructive {
			return true
		}
	}
	return false
}

// SQL renders the plan as a migration file.
func (p Plan) SQL() string {
	var b strings.Builder
	for _, c := range p.Changes {
		if c.Destructive {
			b.WriteString("-- storm:destructive " + c.Why + "\n")
		}
		if c.NoTransaction {
			b.WriteString(NoTransactionMarker + "\n")
		}
		b.WriteString(c.SQL + "\n")
	}
	return b.String()
}

// Concurrently rewrites the index builds and drops on tables that already
// exist in `live` into their CONCURRENTLY forms, which do not block the
// table's writers while they run. An index on a table the same plan creates
// is left alone: nothing writes to a table that does not exist yet, and the
// concurrent build could not share its transaction anyway.
//
// The rewritten changes are marked NoTransaction, because that is the price:
// CREATE INDEX CONCURRENTLY cannot run inside a transaction block, so each
// has to be applied as a single statement outside one.
//
// On a back end with no non-blocking index build this returns the plan
// unchanged rather than approximating one. SQL Server's WITH (ONLINE = ON) is
// Enterprise-only; see the seam's field comment.
func (p Plan) Concurrently(live *schema.Schema) Plan {
	d, err := ddlFor(p.dialect, nil)
	if err != nil || d.CreateIndexConcurrently == nil {
		return p
	}
	out := Plan{Changes: make([]Change, 0, len(p.Changes)), dialect: p.dialect}
	for _, c := range p.Changes {
		switch {
		case c.index != nil && c.table != nil && live != nil && live.Table(c.table.Name) != nil:
			c.SQL = d.CreateIndexConcurrently(c.table, c.index)
			c.NoTransaction = true
		case c.dropIndex != "":
			c.SQL = d.DropIndexConcurrently(c.dropIndex)
			c.NoTransaction = true
		}
		out.Changes = append(out.Changes, c)
	}
	return out
}

// AddsEnumValue reports whether this step is an ALTER TYPE ... ADD VALUE.
//
// Anything applying a Plan has to keep such a step out of the transaction that
// USES the new label: PostgreSQL runs the addition inside a transaction but
// refuses the use until it commits (SQLSTATE 55P04). migrate.Auto gives them a
// leading transaction of their own; `storm diff` gives them a file of their own.
func (c Change) AddsEnumValue() bool { return c.addsEnumValue }

// Diff computes the changes that take `from` to `to` in PostgreSQL. Both are
// normalised first, so the result does not depend on declaration order.
//
// It returns no error because it cannot fail: every renderer in postgresDDL
// returns a literal nil, and the enum step has nothing to refuse. DiffFor is
// the form to use for any other target. TestPostgresRenderersCannotFail holds
// that property in place.
func Diff(from, to *schema.Schema) Plan {
	p, _ := DiffFor(from, to, Postgres)
	return p
}

// DiffFor computes the changes that take `from` to `to` in one dialect.
//
// Both schemas must already be in that dialect's CATALOGUE form — what the
// server stores, not what the model declares. For PostgreSQL that means
// Normalize; for SQL Server, NormalizeMSSQL. An un-normalised model diffs
// against a live database as a long list of changes that are not real, which
// is the one failure mode a migration tool must not have.
//
// The error is a RENDERING failure — a model the target cannot express — and
// the first one wins. Run the dialect's own Check first if you want all of
// them at once; that is what it is for.
func DiffFor(from, to *schema.Schema, dialect Dialect) (Plan, error) {
	if from == nil {
		from = &schema.Schema{}
	}
	if to == nil {
		to = &schema.Schema{}
	}
	from.Normalize()
	to.Normalize()

	d, err := ddlFor(dialect, enumsOf(to))
	if err != nil {
		return Plan{}, err
	}
	b := &planner{d: d}
	b.p.dialect = dialect

	// Enums before tables that use them.
	if d.Enums != nil {
		if err := d.Enums(&b.p, from, to); err != nil {
			return Plan{}, err
		}
	}

	// New and changed tables. Foreign keys for new tables are held back until
	// every table exists — a plan that adds comments.post_id before posts is
	// created is a plan that fails halfway through a deployment.
	var deferredFKs []Change
	for _, t := range to.Tables {
		if t.PartitionOf != "" {
			continue // see dropped tables below
		}
		old := from.Table(t.Name)
		if old == nil {
			b.add(Change{SQL: b.sql(d.CreateTable(t))})
			for _, fk := range t.ForeignKeys {
				deferredFKs = append(deferredFKs, Change{SQL: d.AddForeignKey(t, fk)})
			}
			continue
		}
		diffTable(b, old, t)
	}
	b.p.Changes = append(b.p.Changes, deferredFKs...)

	// The SQL-bodied objects come after every table exists and before anything
	// is dropped: a function's body reads tables, and a view over a table that
	// is about to go has to be dropped before the table, not after.
	//
	// PostgreSQL-only, and gated on the dialect rather than on the schemas
	// being empty: schema has no routine model for any other target yet, so a
	// SQL Server plan that reached here would render PostgreSQL function
	// bodies from whatever a future introspector started filling in.
	if d.DropEnums != nil {
		addRoutines(&b.p, from, to)
		dropRoutines(&b.p, from, to)
	}

	// Dropped tables, after the rest so foreign keys pointing at them are gone.
	for _, t := range from.Tables {
		// A PARTITION is never dropped for being absent from the model, and
		// never created from one.
		//
		// Partitions are routinely made by a scheduled job — one per month is
		// the ordinary shape — so they exist in the database and in no model,
		// which is indistinguishable from an ordinary table somebody deleted a
		// declaration for. Treating them alike means a diff whose first
		// suggestion is to drop last month's audit log. The parent is the
		// declared thing; its partitions are data.
		if t.PartitionOf != "" {
			continue
		}
		if to.Table(t.Name) == nil {
			b.add(Change{
				SQL:         d.DropTable(t),
				Destructive: true,
				Why:         "table " + t.Name + " is no longer in the model",
			})
		}
	}
	// Dropped enums last: a table using one may have just been dropped.
	if d.DropEnums != nil {
		d.DropEnums(&b.p, from, to)
	}
	return b.p, b.err
}

// planner carries the plan being built, the renderers building it, and the
// first rendering failure.
//
// The alternative to carrying the error is to check it at every call site or
// to drop it, and dropping it is how a migration comes to be silently missing
// a table. Once a render has failed nothing more is collected: the plan is
// about to be discarded, and a partial one is worse than none.
type planner struct {
	p   Plan
	d   ddl
	err error
}

func (b *planner) add(c Change) {
	if b.err != nil {
		return
	}
	b.p.add(c)
}

// sql records a rendering failure and passes the statement through, so a call
// site reads as one expression.
func (b *planner) sql(s string, err error) string {
	if err != nil && b.err == nil {
		b.err = err
	}
	return s
}

func diffTable(b *planner, old, cur *schema.Table) {
	d := b.d
	if partitionDesc(old) != partitionDesc(cur) {
		b.add(Change{
			SQL: "-- cannot change partitioning of " + cur.Name + " in place: " +
				partitionDesc(old) + " -> " + partitionDesc(cur),
			Destructive: true,
			Why: "changing a table's partitioning means creating a new table, copying the rows " +
				"and swapping the names, which storm will not do for you",
		})
		return
	}

	// Columns added.
	for _, c := range cur.Columns {
		oc := old.Column(c.Name)
		if oc == nil {
			ch := Change{SQL: b.sql(d.AddColumn(cur, c))}
			if c.NotNull && c.Default == "" && c.Generated == "" && !c.Identity {
				ch.Destructive = true
				ch.Why = "adding NOT NULL column " + c.Name + " with no default fails if the table has rows"
			}
			b.add(ch)
			continue
		}
		diffColumn(b, cur, oc, c)
	}
	// Columns removed.
	for _, oc := range old.Columns {
		if cur.Column(oc.Name) == nil {
			b.add(Change{
				SQL:         d.DropColumn(cur, oc),
				Destructive: true,
				Why:         "column " + cur.Name + "." + oc.Name + " is no longer in the model",
			})
		}
	}

	diffNamed(b, cur, old.Uniques, cur.Uniques,
		func(u *schema.Unique) string { return u.Name },
		func(u *schema.Unique) Change { return Change{SQL: d.AddUnique(cur, u)} },
		func(u *schema.Unique) Change { return Change{SQL: d.DropConstraint(cur, u.Name)} },
		func(a, b *schema.Unique) bool { return eq(a.Columns, b.Columns) },
		"unique constraint")

	diffNamed(b, cur, old.Checks, cur.Checks,
		func(c *schema.Check) string { return c.Name },
		func(c *schema.Check) Change { return Change{SQL: d.AddCheck(cur, c)} },
		func(c *schema.Check) Change { return Change{SQL: d.DropConstraint(cur, c.Name)} },
		func(a, b *schema.Check) bool { return canonical(a.Expr) == canonical(b.Expr) },
		"check constraint")

	diffNamed(b, cur, old.ForeignKeys, cur.ForeignKeys,
		func(f *schema.ForeignKey) string { return f.Name },
		func(f *schema.ForeignKey) Change { return Change{SQL: d.AddForeignKey(cur, f)} },
		func(f *schema.ForeignKey) Change { return Change{SQL: d.DropConstraint(cur, f.Name)} },
		func(a, b *schema.ForeignKey) bool {
			return eq(a.Columns, b.Columns) && a.RefTable == b.RefTable &&
				eq(a.RefColumns, b.RefColumns) && a.OnDelete == b.OnDelete && a.OnUpdate == b.OnUpdate
		},
		"foreign key")

	diffNamed(b, cur, old.Indexes, cur.Indexes,
		func(i *schema.Index) string { return i.Name },
		func(i *schema.Index) Change {
			return Change{SQL: d.CreateIndex(cur, i), table: cur, index: i}
		},
		func(i *schema.Index) Change {
			return Change{SQL: d.DropIndex(cur, i), dropIndex: i.Name}
		},
		func(a, b *schema.Index) bool {
			return a.Unique == b.Unique && a.Method == b.Method &&
				canonical(a.Where) == canonical(b.Where) && sameKeys(a.Columns, b.Columns) &&
				eq(a.Include, b.Include) && a.NullsNotDistinct == b.NullsNotDistinct &&
				sameParams(a.With, b.With)
		},
		"index")
}

func diffColumn(b *planner, t *schema.Table, old, cur *schema.Column) {
	d := b.d
	if !old.Type.Equal(cur.Type) {
		ch := Change{SQL: b.sql(d.AlterColumnType(t, cur))}
		if narrowing(old.Type, cur.Type) {
			ch.Destructive = true
			ch.Why = "narrowing " + cur.Name + " from " + old.Type.SQL() + " to " + cur.Type.SQL() + " can truncate"
		}
		b.add(ch)
	}
	if old.NotNull != cur.NotNull {
		// On a back end whose ALTER COLUMN restates the type, this is the same
		// statement the type change above already emitted. Both are rendered
		// anyway: they are marked differently — only the tightening is
		// destructive — and a migration that applies the same ALTER twice is
		// correct, where one that skipped the second would not be if only the
		// nullability moved.
		if cur.NotNull {
			b.add(Change{
				SQL:         b.sql(d.SetNotNull(t, cur)),
				Destructive: true,
				Why:         "making " + cur.Name + " NOT NULL fails if any existing row is NULL",
			})
		} else {
			b.add(Change{SQL: b.sql(d.DropNotNull(t, cur))})
		}
	}
	if old.Default != cur.Default {
		if cur.Default == "" {
			b.add(Change{SQL: d.DropDefault(t, cur)})
		} else {
			// One change, not a drop and an add, even on a back end where the
			// default is a CONSTRAINT and replacing one means both: see
			// ddl_mssql.go's SetDefault. Which statements that takes is the
			// renderer's business, and putting it here would make the shared
			// loop emit a step PostgreSQL does not need.
			b.add(Change{SQL: d.SetDefault(t, cur)})
		}
	}
}

// diffNamed is the add/drop/replace loop shared by every named constraint kind.
func diffNamed[T any](b *planner, t *schema.Table, old, cur []T,
	name func(T) string, add func(T) Change, drop func(T) Change,
	same func(a, b T) bool, kind string) {

	byName := map[string]T{}
	for _, o := range old {
		byName[name(o)] = o
	}
	seen := map[string]bool{}
	for _, c := range cur {
		seen[name(c)] = true
		o, ok := byName[name(c)]
		if !ok {
			b.add(add(c))
			continue
		}
		if !same(o, c) {
			// No back end storm targets can alter these in place; drop and
			// recreate.
			b.add(drop(o))
			b.add(add(c))
		}
	}
	for _, o := range old {
		if !seen[name(o)] {
			x := drop(o)
			x.Destructive = true
			x.Why = kind + " " + name(o) + " on " + t.Name + " is no longer in the model"
			b.add(x)
		}
	}
}

func (p *Plan) add(c Change) { p.Changes = append(p.Changes, c) }

// narrowing reports whether a type change can lose information.
func narrowing(from, to schema.Type) bool {
	if from.Name != to.Name {
		return true // any cross-type change needs a human
	}
	if from.Size > 0 && to.Size > 0 && to.Size < from.Size {
		return true
	}
	if from.Size > 0 && to.Size == 0 {
		return false // widening to unbounded
	}
	if from.Precision > 0 && to.Precision > 0 &&
		(to.Precision < from.Precision || to.Scale < from.Scale) {
		return true
	}
	return false
}

// canonical strips whitespace and outer parens so two spellings of the same
// expression compare equal. Postgres rewrites everything it stores, so exact
// text comparison would report differences that are not real.
func canonical(s string) string {
	s = stripCasts(s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > 1 && s[0] == '(' && s[len(s)-1] == ')' {
		depth, ok := 0, true
		for i := 0; i < len(s); i++ {
			if s[i] == '(' {
				depth++
			} else if s[i] == ')' {
				depth--
				if depth == 0 && i != len(s)-1 {
					ok = false
				}
			}
		}
		if !ok {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

func sameKeys(a, b []schema.IndexColumn) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if canonical(a[i].Name) != canonical(b[i].Name) ||
			a[i].Desc != b[i].Desc || a[i].NullsLast != b[i].NullsLast ||
			a[i].NullsFirst != b[i].NullsFirst ||
			bareOpClass(a[i].OpClass) != bareOpClass(b[i].OpClass) || a[i].Collate != b[i].Collate ||
			a[i].Prefix != b[i].Prefix {
			return false
		}
	}
	return true
}

// bareOpClass strips a schema qualifier: the model may say where a class
// lives, the server prints that only when it is off the search path, and the
// two are the same class either way.
func bareOpClass(class string) string {
	if dot := strings.LastIndexByte(class, '.'); dot >= 0 {
		return class[dot+1:]
	}
	return class
}

// sameParams compares storage parameters by name and value, in order: the
// order is the model's and the database prints it back the same way.
func sameParams(a, b []schema.StorageParam) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func identList(q func(string) string, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = q(n)
	}
	return strings.Join(out, ", ")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// stripCasts removes ::type suffixes. PostgreSQL adds them to everything it
// stores ('pending' becomes 'pending'::status), and they never change meaning.
// This makes offline diffs — model against a checked-in snapshot — far less
// noisy. It is not a substitute for Normalize: only PostgreSQL knows that
// BETWEEN is two comparisons.
func stripCasts(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && i+1 < len(s) && s[i+1] == ':' {
			i += 2
			for i < len(s) {
				c := s[i]
				isIdent := c == '_' || c == '.' || c == ' ' ||
					c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
				if !isIdent {
					break
				}
				i++
			}
			i--
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// partitionDesc renders a table's partitioning for comparison and for the
// message that explains a mismatch.
func partitionDesc(t *schema.Table) string {
	if t == nil || t.Partition == nil {
		return "not partitioned"
	}
	return "partitioned by " + t.Partition.Strategy +
		" (" + strings.Join(t.Partition.Columns, ", ") + ")"
}
