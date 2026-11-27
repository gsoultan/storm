// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

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
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gsoultan/storm/v2/compile/pgddl"
	"github.com/gsoultan/storm/v2/schema"
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
	// Object is the kind of thing the step adds, changes or drops: one of
	// the Object constants, or "" for a step that is none of them, such as
	// an enum's or a routine's. AutoOptions.Accept reads it to let through
	// the destructive steps of one kind.
	Object string
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

	// constraint names a CHECK or FOREIGN KEY this change ADDS, so that
	// Concurrently can split it into ADD ... NOT VALID and a VALIDATE.
	constraint string

	// validates marks ALTER TABLE ... VALIDATE CONSTRAINT. It can run in a
	// transaction, but should not share one with the ADD it follows: the
	// point of the split is that the scan runs without the ADD's lock, and a
	// transaction holds every lock it took until it ends.
	validates bool

	// addsEnumValue marks ALTER TYPE ... ADD VALUE. PostgreSQL will run it
	// inside a transaction but refuses to let anything USE the new label until
	// that transaction commits (SQLSTATE 55P04, "unsafe use of new value"), so
	// a step that adds a label and a step that gives a column a default of it
	// cannot share one. Auto commits these first, on their own; anything
	// applying a Plan as a single transaction has to do the same.
	addsEnumValue bool
}

// The kinds of thing a Change works on, for Change.Object.
const (
	ObjectTable      = "table"
	ObjectColumn     = "column"
	ObjectIndex      = "index"
	ObjectUnique     = "unique constraint"
	ObjectCheck      = "check constraint"
	ObjectForeignKey = "foreign key"
	ObjectPrimaryKey = "primary key"
	ObjectExclude    = "exclusion constraint"
)

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

	// from and to are what the plan was computed between, kept so Reverse
	// can compute the plan back, and concurrent whether Concurrently has
	// rewritten it, so the plan back is written in the same form.
	from, to   *schema.Schema
	ren        renames
	concurrent bool
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

// Concurrently rewrites the plan's changes to tables that already exist in
// `live` into their online forms, which do not block the table's writers while
// they run: index builds and drops become CREATE/DROP INDEX CONCURRENTLY, and a
// CHECK or FOREIGN KEY becomes ADD ... NOT VALID followed, at the end of the
// plan, by a VALIDATE CONSTRAINT that scans the rows without blocking writes. An index on a table the same plan creates
// is left alone: nothing writes to a table that does not exist yet, and the
// concurrent build could not share its transaction anyway.
//
// The rewritten index changes are marked NoTransaction, because that is the price:
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
	out := p
	out.Changes = make([]Change, 0, len(p.Changes))
	out.concurrent = true
	var validate []Change
	exists := func(t *schema.Table) bool { return t != nil && live != nil && live.Table(t.Name) != nil }
	for _, c := range p.Changes {
		switch {
		case c.index != nil && exists(c.table):
			c.SQL = d.CreateIndexConcurrently(c.table, c.index)
			c.NoTransaction = true
		case c.dropIndex != "":
			c.SQL = d.DropIndexConcurrently(c.dropIndex)
			c.NoTransaction = true
		case c.constraint != "" && d.NotValid != nil && exists(c.table):
			c.SQL = d.NotValid(c.SQL)
			validate = append(validate, Change{
				SQL:       d.ValidateConstraint(c.table, c.constraint),
				Object:    c.Object,
				validates: true,
			})
		}
		out.Changes = append(out.Changes, c)
	}
	out.Changes = append(out.Changes, validate...)
	return out
}

// Reverse is the plan that takes the database back from where this plan
// leaves it to where it found it — the body of a down migration.
//
// It is the same differ run the other way, not an inversion of each step, so
// it is exactly as careful as the forward plan: a table the forward plan
// creates is a table the reverse plan DROPS, and that step is marked
// destructive, because every row written since the up migration ran goes with
// it. A down migration loses data by construction; the marks say where.
//
// Some forward steps have no reverse. PostgreSQL cannot remove an enum label,
// so a reversed label addition is a comment saying so, marked destructive like
// every step a human has to finish.
//
// A plan that was not computed by DiffFor — one built by hand from a slice of
// changes — has nothing to reverse from, and Reverse refuses it.
func (p Plan) Reverse() (Plan, error) {
	if p.from == nil || p.to == nil {
		return Plan{}, fmt.Errorf("migrate: Reverse needs a plan computed by Diff or DiffFor")
	}
	back, err := diffFor(p.to, p.from, p.dialect, p.ren.invert())
	if err != nil {
		return Plan{}, err
	}
	if p.concurrent {
		back = back.Concurrently(p.to)
	}
	return back, nil
}

// Index names the index this step builds or drops, and says which, or is ""
// for a step that is neither. It is how a down migration pairs with the up
// migration it undoes when each holds one CREATE/DROP INDEX CONCURRENTLY.
func (c Change) Index() (name string, build bool) {
	switch {
	case c.index != nil:
		return c.index.Name, true
	case c.dropIndex != "":
		return c.dropIndex, false
	}
	return "", false
}

// Validates reports whether this step is an ALTER TABLE ... VALIDATE
// CONSTRAINT, which anything applying a Plan should run in a transaction of its
// own, after the one that added the constraint: see Concurrently.
func (c Change) Validates() bool { return c.validates }

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
	return diffFor(from, to, dialect, renamesOf(from, to))
}

// diffFor is DiffFor with the renames already decided, which is what lets
// Reverse run the same differ the other way: the plan back renames the new
// names to the old ones, and nothing in either schema says so.
//
// The plan runs in phases, because a step can only run once what it depends on
// is in place and before what depends on it goes:
//
//  1. enum types and labels, which columns are about to use;
//  2. views over a table whose columns are about to be dropped or retyped,
//     which PostgreSQL refuses to do under a view;
//  3. foreign keys that are going or being replaced, before the keys they
//     reference and the columns they are made of;
//  4. table renames, then each table: new tables, and for an existing one its
//     column renames, the constraints and indexes that are going, its columns,
//     and the constraints and indexes that are new;
//  5. every new or replaced foreign key, once every table and key it needs
//     exists;
//  6. routines, then the views of phase 2 again, then dropped tables and enums.
func diffFor(from, to *schema.Schema, dialect Dialect, ren renames) (Plan, error) {
	d, err := ddlFor(dialect, enumsOf(to))
	if err != nil {
		return Plan{}, err
	}
	b := &planner{d: d, ren: ren}
	b.p.dialect = dialect
	b.p.from, b.p.to, b.p.ren = from, to, ren

	if d.Enums != nil {
		if err := d.Enums(&b.p, from, to); err != nil {
			return Plan{}, err
		}
	}

	// From here on `from` is read as it stands once every rename has run, so
	// a renamed table or column compares by its new name, and so does every
	// key and index that names it.
	old := ren.apply(from)

	// What each surviving table rebuilds — columns dropped and added again,
	// or freed for an ALTER — and which of its keys go. Both are needed before
	// the first statement: a foreign key into either has to go first.
	type pair struct{ old, cur *schema.Table }
	var pairs []pair
	rebuilt := map[string]map[string]bool{}
	keys := map[string][][]string{}
	for _, t := range to.Tables {
		if t.PartitionOf != "" {
			continue
		}
		if o := old.Table(t.Name); o != nil && partitionDesc(o) == partitionDesc(t) {
			pairs = append(pairs, pair{o, t})
			rebuilt[t.Name] = rebuiltColumns(d, o, t)
			keys[t.Name] = droppedKeys(o, t, rebuilt[t.Name])
		}
	}
	dropped := map[string]bool{}
	for _, t := range old.Tables {
		if t.PartitionOf == "" && to.Table(t.Name) == nil {
			dropped[t.Name] = true
		}
	}

	// Phase 2.
	var views []string
	if d.DropEnums != nil {
		affected := map[string]bool{}
		for _, p := range pairs {
			if columnsShrink(p.old, p.cur, rebuilt[p.cur.Name]) {
				affected[p.cur.Name] = true
			}
		}
		views = viewsToRebuild(old, affected)
		for _, v := range views {
			c := Change{SQL: pgddl.DropView(v)}
			if to.View(v) == nil {
				c.Destructive, c.Why = true, "view "+v+" is no longer in the model"
			}
			b.add(c)
		}
	}

	// Phase 3, collecting phase 5 as it goes.
	var fkAdds []Change
	for _, p := range pairs {
		forced := func(fk *schema.ForeignKey) bool {
			return touches(rebuilt[p.cur.Name], fk.Columns) ||
				touches(rebuilt[fk.RefTable], fk.RefColumns) ||
				keyIn(keys[fk.RefTable], fk.RefColumns)
		}
		for _, of := range p.old.ForeignKeys {
			cf := findFK(p.cur.ForeignKeys, of.Name)
			switch {
			case cf == nil:
				b.add(Change{
					SQL:         d.DropConstraint(p.cur, of.Name),
					Object:      ObjectForeignKey,
					Destructive: true,
					Why:         "foreign key " + of.Name + " on " + p.cur.Name + " is no longer in the model",
				})
			case !sameFK(of, cf) || forced(cf):
				b.add(Change{SQL: d.DropConstraint(p.cur, of.Name), Object: ObjectForeignKey})
				fkAdds = append(fkAdds, addFK(d, p.cur, cf))
			case of.NotValid && d.ValidateConstraint != nil:
				// Held NOT VALID and declared as it stands: validated, not
				// dropped and re-added. The drop would leave a window with
				// no constraint at all, and the re-add would scan the table
				// under the lock the NOT VALID was there to avoid.
				fkAdds = append(fkAdds, Change{SQL: d.ValidateConstraint(p.cur, cf.Name), Object: ObjectForeignKey, validates: true})
			}
		}
		for _, cf := range p.cur.ForeignKeys {
			if findFK(p.old.ForeignKeys, cf.Name) == nil {
				fkAdds = append(fkAdds, addFK(d, p.cur, cf))
			}
		}
	}
	// A dropped table can be referenced by another dropped table, and the
	// second drop would then fail on the first: those keys go first.
	for _, t := range old.Tables {
		if !dropped[t.Name] {
			continue
		}
		for _, fk := range t.ForeignKeys {
			if fk.RefTable != t.Name && dropped[fk.RefTable] {
				b.add(Change{SQL: d.DropConstraint(t, fk.Name), Object: ObjectForeignKey})
			}
		}
	}

	// Phase 4.
	for _, o := range ren.tableOrder() {
		b.add(Change{SQL: d.RenameTable(o, ren.table(o)), Object: ObjectTable})
	}
	var newFKs []Change
	for _, t := range to.Tables {
		if t.PartitionOf != "" {
			continue // see dropped tables below
		}
		o := old.Table(t.Name)
		if o == nil {
			b.add(Change{SQL: b.sql(d.CreateTable(t)), Object: ObjectTable})
			for _, fk := range t.ForeignKeys {
				newFKs = append(newFKs, Change{SQL: d.AddForeignKey(t, fk), Object: ObjectForeignKey})
			}
			continue
		}
		diffTable(b, o, t, rebuilt[t.Name])
	}

	// Phase 5. Foreign keys for new tables come first, as they always have:
	// a plan that adds comments.post_id before posts is created is a plan
	// that fails halfway through a deployment, and so is one that adds a key
	// on an existing table to a table created after it.
	b.p.Changes = append(b.p.Changes, newFKs...)
	b.p.Changes = append(b.p.Changes, fkAdds...)

	// Phase 6. The SQL-bodied objects come after every table exists and
	// before anything is dropped: a function's body reads tables, and a view
	// over a table that is about to go has to be dropped before the table,
	// not after.
	//
	// PostgreSQL-only, and gated on the dialect rather than on the schemas
	// being empty: schema has no routine model for any other target yet, so a
	// SQL Server plan that reached here would render PostgreSQL function
	// bodies from whatever a future introspector started filling in.
	if d.DropEnums != nil {
		gone := map[string]bool{}
		for _, v := range views {
			gone[v] = true
		}
		addRoutines(&b.p, from, to, gone)
		dropRoutines(&b.p, from, to, gone)
	}

	// Dropped tables, after the rest so foreign keys pointing at them are gone.
	for _, t := range old.Tables {
		// A PARTITION is never dropped for being absent from the model, and
		// never created from one.
		//
		// Partitions are routinely made by a scheduled job — one per month is
		// the ordinary shape — so they exist in the database and in no model,
		// which is indistinguishable from an ordinary table somebody deleted a
		// declaration for. Treating them alike means a diff whose first
		// suggestion is to drop last month's audit log. The parent is the
		// declared thing; its partitions are data.
		if dropped[t.Name] {
			b.add(Change{
				SQL:         d.DropTable(t),
				Object:      ObjectTable,
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

// addFK is the ADD of a foreign key on a table that exists, carrying what
// Concurrently needs to add it NOT VALID.
func addFK(d ddl, t *schema.Table, fk *schema.ForeignKey) Change {
	return Change{SQL: d.AddForeignKey(t, fk), Object: ObjectForeignKey, table: t, constraint: fk.Name}
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
	ren renames
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

// diffTable takes old to cur. old is the table as it stands after every
// rename, and rebuilt names the columns whose indexes and constraints have
// to be recreated around the change to the column.
func diffTable(b *planner, old, cur *schema.Table, rebuilt map[string]bool) {
	d := b.d

	// Partitioning cannot be altered. A table is created partitioned or it is
	// not, so the only honest plan is to say so and stop: an ALTER that
	// silently did nothing would leave a schema that looks migrated and
	// routes every row to the wrong place.
	if partitionDesc(old) != partitionDesc(cur) {
		b.add(Change{
			SQL: "-- cannot change the partitioning of " + cur.Name +
				": PostgreSQL has no ALTER TABLE ... PARTITION BY",
			Destructive: true,
			Why: "table " + cur.Name + " is " + partitionDesc(old) + " in the database and " +
				partitionDesc(cur) + " in the model; this needs a new table and a data move",
		})
		return
	}

	// Columns renamed, first: everything after this names them as they will
	// be called.
	for _, c := range cur.Columns {
		if from, ok := b.ren.oldColumn(cur.Name, c.Name); ok {
			b.add(Change{SQL: d.RenameColumn(cur, from, c.Name), Object: ObjectColumn})
		}
	}

	// Every constraint and index is compared, and the ones going are dropped
	// BEFORE any column changes: a dropped column takes its indexes and
	// constraints with it, and a later DROP INDEX for one of them fails on an
	// index that is no longer there. The ones arriving are added after, when
	// every column they name exists.
	var drops, adds []Change
	collect := func(dr, ad []Change) { drops, adds = append(drops, dr...), append(adds, ad...) }

	collect(diffNamed(cur, old.Excludes, cur.Excludes,
		func(x *schema.Exclude) string { return x.Name },
		func(x *schema.Exclude) Change { return Change{SQL: d.AddExclude(cur, x)} },
		func(x *schema.Exclude) Change { return Change{SQL: d.DropConstraint(cur, x.Name)} },
		func(a, b *schema.Exclude) bool { return sameExclude(a, b) && !excludeTouches(rebuilt, b) },
		ObjectExclude))

	collect(diffNamed(cur, old.Checks, cur.Checks,
		func(c *schema.Check) string { return c.Name },
		func(c *schema.Check) Change { return Change{SQL: d.AddCheck(cur, c), table: cur, constraint: c.Name} },
		func(c *schema.Check) Change { return Change{SQL: d.DropConstraint(cur, c.Name)} },
		func(a, b *schema.Check) bool {
			return canonical(a.Expr) == canonical(b.Expr) && !exprTouches(rebuilt, b.Expr)
		},
		ObjectCheck))

	collect(diffNamed(cur, old.Uniques, cur.Uniques,
		func(u *schema.Unique) string { return u.Name },
		func(u *schema.Unique) Change { return Change{SQL: d.AddUnique(cur, u)} },
		func(u *schema.Unique) Change { return Change{SQL: d.DropConstraint(cur, u.Name)} },
		func(a, b *schema.Unique) bool { return sameUnique(a, b) && !touches(rebuilt, b.Columns) },
		ObjectUnique))

	collect(diffNamed(cur, old.Indexes, cur.Indexes,
		func(i *schema.Index) string { return i.Name },
		func(i *schema.Index) Change {
			return Change{SQL: d.CreateIndex(cur, i), table: cur, index: i}
		},
		func(i *schema.Index) Change {
			return Change{SQL: d.DropIndex(cur, i), dropIndex: i.Name}
		},
		func(a, b *schema.Index) bool { return sameIndex(a, b) && !indexTouches(rebuilt, b) },
		ObjectIndex))

	// The primary key, which has no name the model knows and so is compared
	// by its columns.
	pkChanged := !samePK(old, cur, rebuilt)
	if pkChanged && len(old.PrimaryKey) > 0 {
		c := Change{SQL: d.DropPrimaryKey(cur), Object: ObjectPrimaryKey}
		if len(cur.PrimaryKey) == 0 {
			c.Destructive = true
			c.Why = "primary key of " + cur.Name + " is no longer in the model"
		}
		drops = append([]Change{c}, drops...)
	}
	if pkChanged && len(cur.PrimaryKey) > 0 {
		adds = append([]Change{{SQL: d.AddPrimaryKey(cur), Object: ObjectPrimaryKey}}, adds...)
	}

	// An index on a column this plan drops, or drops and adds again, is
	// dropped in the plan's transaction, never CONCURRENTLY after it: by the
	// time a concurrent drop ran, the column's drop would have taken the
	// index with it, and the DROP INDEX would fail on nothing.
	gone := map[string]bool{}
	maps.Copy(gone, rebuilt)
	for _, oc := range old.Columns {
		if cur.Column(oc.Name) == nil {
			gone[oc.Name] = true
		}
	}
	for i := range drops {
		if ix := findIndex(old.Indexes, drops[i].dropIndex); ix != nil && indexTouches(gone, ix) {
			drops[i].dropIndex = ""
		}
	}
	for _, c := range drops {
		b.add(c)
	}

	// Columns added.
	for _, c := range cur.Columns {
		oc := old.Column(c.Name)
		if oc == nil {
			addColumn(b, cur, c)
			continue
		}
		diffColumn(b, cur, oc, c)
	}
	// Columns removed.
	for _, oc := range old.Columns {
		if cur.Column(oc.Name) == nil {
			b.add(Change{
				SQL:         d.DropColumn(cur, oc),
				Object:      ObjectColumn,
				Destructive: true,
				Why:         "column " + cur.Name + "." + oc.Name + " is no longer in the model",
			})
		}
	}

	for _, c := range adds {
		b.add(c)
	}

	// A check the database holds NOT VALID and the model declares as it
	// stands is validated, not dropped and re-added; see phase 3 for the
	// foreign keys. One that differs from the model has already been replaced
	// above, by a constraint that is valid.
	if d.ValidateConstraint != nil {
		for _, c := range cur.Checks {
			if o := findCheck(old.Checks, c.Name); o != nil && o.NotValid &&
				canonical(o.Expr) == canonical(c.Expr) && !exprTouches(rebuilt, c.Expr) {
				b.add(Change{SQL: d.ValidateConstraint(cur, c.Name), Object: ObjectCheck, validates: true})
			}
		}
	}

	if d.CommentTable != nil && old.Comment != cur.Comment {
		b.add(Change{SQL: d.CommentTable(cur), Object: ObjectTable})
	}
}

// addColumn adds c, with its comment, marking the step destructive when it
// cannot succeed on a table with rows.
func addColumn(b *planner, t *schema.Table, c *schema.Column) {
	ch := Change{SQL: b.sql(b.d.AddColumn(t, c)), Object: ObjectColumn}
	if c.NotNull && c.Default == "" && c.Generated == "" && !c.Identity && !c.Serial {
		ch.Destructive = true
		ch.Why = "adding NOT NULL column " + c.Name + " with no default fails if the table has rows"
	}
	b.add(ch)
	if b.d.CommentColumn != nil && c.Comment != "" {
		b.add(Change{SQL: b.d.CommentColumn(t, c), Object: ObjectColumn})
	}
}

func diffColumn(b *planner, t *schema.Table, old, cur *schema.Column) {
	d := b.d

	// A generated column whose expression changes, or that a plain column
	// becomes, is dropped and added again: there is no portable ALTER for
	// either, and the values are the expression's anyway.
	if regenerates(d, old, cur) {
		drop := Change{SQL: d.DropColumn(t, old), Object: ObjectColumn}
		switch {
		case old.Generated == "":
			drop.Destructive, drop.Why = true, "column "+t.Name+"."+cur.Name+
				" becomes generated, so the values it holds are replaced by the expression's"
		case cur.Generated == "":
			drop.Destructive, drop.Why = true, "column "+t.Name+"."+cur.Name+
				" stops being generated, and this server cannot keep the values it computed"
		}
		b.add(drop)
		addColumn(b, t, cur)
		return
	}
	if old.Generated != "" && cur.Generated == "" {
		b.add(Change{SQL: d.DropExpression(t, cur), Object: ObjectColumn})
	}

	// What owns a sequence goes before anything is retyped, and the new
	// owner arrives after the default it replaces has gone.
	if old.Identity && !cur.Identity {
		if d.DropIdentity == nil {
			b.add(cannotInPlace(t, cur, "stop being an identity column"))
		} else {
			b.add(Change{SQL: d.DropIdentity(t, cur), Object: ObjectColumn})
		}
	}
	if old.Serial && !cur.Serial {
		if d.DropSerial == nil {
			b.add(cannotInPlace(t, cur, "stop being a serial"))
		} else {
			for _, s := range d.DropSerial(t, cur) {
				b.add(Change{SQL: s, Object: ObjectColumn})
			}
		}
	}

	retyped := !old.Type.Equal(cur.Type)
	converted := retyped && cur.ConvertFrom != ""
	// Where ALTER COLUMN refuses a column a DEFAULT is bound to, the default
	// goes first and comes back after, as it does for a conversion.
	freed := retyped && !converted && d.AlterFreesColumn && old.Default != ""
	if converted {
		convertColumn(b, t, old, cur)
	} else if retyped {
		if freed {
			b.add(Change{SQL: d.DropDefault(t, old), Object: ObjectColumn})
		}
		ch := Change{SQL: b.sql(d.AlterColumnType(t, cur)), Object: ObjectColumn}
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
		if cur.NotNull && cur.FillNulls != "" {
			// The model says what the NULL rows become, so tightening cannot
			// fail on one: the fill and the constraint are one step to review.
			b.add(Change{SQL: d.FillNulls(t, cur), Object: ObjectColumn})
			b.add(Change{SQL: b.sql(d.SetNotNull(t, cur)), Object: ObjectColumn})
		} else if cur.NotNull {
			b.add(Change{
				SQL:         b.sql(d.SetNotNull(t, cur)),
				Object:      ObjectColumn,
				Destructive: true,
				Why:         "making " + cur.Name + " NOT NULL fails if any existing row is NULL",
			})
		} else {
			b.add(Change{SQL: b.sql(d.DropNotNull(t, cur)), Object: ObjectColumn})
		}
	}
	switch {
	case converted:
		// convertColumn has put the default back.
	case freed:
		if cur.Default != "" {
			b.add(Change{SQL: d.SetDefault(t, cur), Object: ObjectColumn})
		}
	case old.Default != cur.Default:
		if cur.Default == "" {
			b.add(Change{SQL: d.DropDefault(t, cur), Object: ObjectColumn})
		} else {
			// One change, not a drop and an add, even on a back end where the
			// default is a CONSTRAINT and replacing one means both: see
			// ddl_mssql.go's SetDefault. Which statements that takes is the
			// renderer's business, and putting it here would make the shared
			// loop emit a step PostgreSQL does not need.
			b.add(Change{SQL: d.SetDefault(t, cur), Object: ObjectColumn})
		}
	}

	if cur.Serial && !old.Serial {
		if d.AddSerial == nil {
			b.add(cannotInPlace(t, cur, "become a serial"))
		} else {
			for _, s := range d.AddSerial(t, cur) {
				b.add(Change{SQL: s, Object: ObjectColumn})
			}
		}
	}
	if cur.Identity && !old.Identity {
		if d.AddIdentity == nil {
			b.add(cannotInPlace(t, cur, "become an identity column"))
		} else {
			for _, s := range d.AddIdentity(t, cur) {
				b.add(Change{SQL: s, Object: ObjectColumn})
			}
		}
	}

	if d.CommentColumn != nil && old.Comment != cur.Comment {
		b.add(Change{SQL: d.CommentColumn(t, cur), Object: ObjectColumn})
	}
}

// cannotInPlace is the step a human has to finish: a change the server has no
// ALTER for. It is a comment, marked destructive like every such step, so a
// plan holding one is never applied unreviewed.
func cannotInPlace(t *schema.Table, c *schema.Column, what string) Change {
	return Change{
		SQL:         "-- cannot make " + t.Name + "." + c.Name + " " + what + " in place on this server",
		Object:      ObjectColumn,
		Destructive: true,
		Why: "column " + t.Name + "." + c.Name + " has to " + what +
			"; this needs a new column and a copy of the data",
	}
}

// regenerates reports whether the change from old to cur drops the column and
// adds it again.
func regenerates(d ddl, old, cur *schema.Column) bool {
	if canonical(old.Generated) == canonical(cur.Generated) {
		return false
	}
	return !(old.Generated != "" && cur.Generated == "" && d.DropExpression != nil)
}

// rebuiltColumns names the columns of cur whose indexes and constraints the
// plan has to recreate: those it drops and adds again, and — where ALTER
// COLUMN refuses a column anything depends on — those it alters.
func rebuiltColumns(d ddl, old, cur *schema.Table) map[string]bool {
	out := map[string]bool{}
	for _, c := range cur.Columns {
		oc := old.Column(c.Name)
		if oc == nil {
			continue
		}
		if regenerates(d, oc, c) ||
			d.AlterFreesColumn && (!oc.Type.Equal(c.Type) || oc.NotNull != c.NotNull) {
			out[c.Name] = true
		}
	}
	return out
}

// droppedKeys lists the column sets of old's keys — its primary key, its
// unique constraints and its unique indexes — that the plan drops, so that a
// foreign key referencing one is dropped before it and added back after.
func droppedKeys(old, cur *schema.Table, rebuilt map[string]bool) [][]string {
	var out [][]string
	if len(old.PrimaryKey) > 0 && !samePK(old, cur, rebuilt) {
		out = append(out, old.PrimaryKey)
	}
	for _, u := range old.Uniques {
		cu := findUnique(cur.Uniques, u.Name)
		if cu == nil || !sameUnique(u, cu) || touches(rebuilt, cu.Columns) {
			out = append(out, u.Columns)
		}
	}
	for _, ix := range old.Indexes {
		if !ix.Unique {
			continue
		}
		ci := findIndex(cur.Indexes, ix.Name)
		if ci == nil || !sameIndex(ix, ci) || indexTouches(rebuilt, ci) {
			cols := make([]string, len(ix.Columns))
			for i, k := range ix.Columns {
				cols[i] = k.Name
			}
			out = append(out, cols)
		}
	}
	return out
}

// columnsShrink reports whether the plan drops or retypes a column of the
// table, which is what PostgreSQL refuses to do under a view that reads it.
func columnsShrink(old, cur *schema.Table, rebuilt map[string]bool) bool {
	if len(rebuilt) > 0 {
		return true
	}
	for _, oc := range old.Columns {
		c := cur.Column(oc.Name)
		if c == nil || !oc.Type.Equal(c.Type) {
			return true
		}
	}
	return false
}

// viewsToRebuild names the views of s that read an affected table, or a view
// that does, ordered so each comes before any view it reads: the order to
// drop them in. Reading is judged by the name appearing in the definition,
// which can only err towards rebuilding a view that did not need it.
func viewsToRebuild(s *schema.Schema, affected map[string]bool) []string {
	if len(affected) == 0 {
		return nil
	}
	in := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, v := range s.Views {
			if in[v.Name] {
				continue
			}
			for name := range affected {
				if mentions(v.Def, name) {
					in[v.Name], affected[v.Name], grew = true, true, true
					break
				}
			}
		}
	}
	// Dependents first: repeatedly take a view no remaining view reads.
	var out []string
	for len(out) < len(in) {
		for _, v := range s.Views {
			if !in[v.Name] || contains(out, v.Name) {
				continue
			}
			read := false
			for _, w := range s.Views {
				if w.Name != v.Name && in[w.Name] && !contains(out, w.Name) && mentions(w.Def, v.Name) {
					read = true
					break
				}
			}
			if !read {
				out = append(out, v.Name)
			}
		}
	}
	return out
}

// mentions reports whether name appears in sql as a whole identifier.
func mentions(sql, name string) bool {
	low, n := strings.ToLower(sql), strings.ToLower(name)
	for i := 0; ; {
		j := strings.Index(low[i:], n)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(n)
		if (j == 0 || !isIdentByte(low[j-1])) && (end == len(low) || !isIdentByte(low[end])) {
			return true
		}
		i = j + 1
	}
}

func samePK(old, cur *schema.Table, rebuilt map[string]bool) bool {
	return eq(old.PrimaryKey, cur.PrimaryKey) && !touches(rebuilt, cur.PrimaryKey)
}

func sameUnique(a, b *schema.Unique) bool { return eq(a.Columns, b.Columns) }

func sameIndex(a, b *schema.Index) bool {
	// An index the server marked invalid is rebuilt whatever its definition
	// says: the planner does not use it, and the drop and the build are the
	// only way back.
	return !a.Invalid && !b.Invalid && a.Unique == b.Unique && a.Method == b.Method &&
		canonical(a.Where) == canonical(b.Where) && sameKeys(a.Columns, b.Columns) &&
		eq(a.Include, b.Include) && a.NullsNotDistinct == b.NullsNotDistinct &&
		sameParams(a.With, b.With)
}

func sameExclude(a, b *schema.Exclude) bool {
	if a.Method != b.Method || canonical(a.Where) != canonical(b.Where) || len(a.Parts) != len(b.Parts) {
		return false
	}
	for i := range a.Parts {
		x, y := a.Parts[i], b.Parts[i]
		if canonical(x.Column) != canonical(y.Column) || x.Expr != y.Expr ||
			strings.TrimSpace(x.Operator) != strings.TrimSpace(y.Operator) {
			return false
		}
	}
	return true
}

// touches reports whether any of cols is rebuilt.
func touches(rebuilt map[string]bool, cols []string) bool {
	for _, c := range cols {
		if rebuilt[c] {
			return true
		}
	}
	return false
}

// exprTouches reports whether an expression names a rebuilt column.
func exprTouches(rebuilt map[string]bool, expr string) bool {
	for c := range rebuilt {
		if mentions(expr, c) {
			return true
		}
	}
	return false
}

func indexTouches(rebuilt map[string]bool, ix *schema.Index) bool {
	if len(rebuilt) == 0 {
		return false
	}
	for _, k := range ix.Columns {
		if rebuilt[k.Name] || k.Expr && exprTouches(rebuilt, k.Name) {
			return true
		}
	}
	return touches(rebuilt, ix.Include) || exprTouches(rebuilt, ix.Where)
}

func excludeTouches(rebuilt map[string]bool, ex *schema.Exclude) bool {
	for _, p := range ex.Parts {
		if rebuilt[p.Column] || p.Expr && exprTouches(rebuilt, p.Column) {
			return true
		}
	}
	return exprTouches(rebuilt, ex.Where)
}

// keyIn reports whether cols, in any order, is one of keys.
func keyIn(keys [][]string, cols []string) bool {
	for _, k := range keys {
		if len(k) != len(cols) {
			continue
		}
		all := true
		for _, c := range cols {
			if !contains(k, c) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// convertColumn changes cur's type with the conversion its ConvertFrom
// names. The old default goes first and the new one comes after: the old
// type's default seldom casts to the new type, and PostgreSQL refuses the
// change when it does not. The type change is not marked destructive, as
// the model says how every value converts and a value that does not fails
// the migration rather than losing data.
func convertColumn(b *planner, t *schema.Table, old, cur *schema.Column) {
	d := b.d
	if d.ConvertHelper == nil {
		b.sql("", fmt.Errorf("migrate: %s.%s: ConvertFrom is PostgreSQL-only", t.Name, cur.Name))
		return
	}
	if old.Default != "" {
		b.add(Change{SQL: d.DropDefault(t, old), Object: ObjectColumn})
	}
	create, drop := d.ConvertHelper(cur)
	if create != "" {
		b.add(Change{SQL: create})
	}
	b.add(Change{SQL: b.sql(d.AlterColumnType(t, cur)), Object: ObjectColumn})
	if drop != "" {
		b.add(Change{SQL: drop})
	}
	if cur.Default != "" {
		b.add(Change{SQL: d.SetDefault(t, cur), Object: ObjectColumn})
	}
}

// diffNamed compares one named kind of constraint or index, and returns the
// drops — of the ones going and the ones being replaced — apart from the adds,
// because the caller runs the column changes between the two.
func diffNamed[T any](t *schema.Table, old, cur []T,
	name func(T) string, add func(T) Change, drop func(T) Change,
	same func(a, b T) bool, kind string) (drops, adds []Change) {

	add, drop = objectOf(add, kind), objectOf(drop, kind)
	byName := map[string]T{}
	for _, o := range old {
		byName[name(o)] = o
	}
	seen := map[string]bool{}
	for _, c := range cur {
		seen[name(c)] = true
		o, ok := byName[name(c)]
		if !ok {
			adds = append(adds, add(c))
			continue
		}
		if !same(o, c) {
			// No back end storm targets can alter these in place; drop and
			// recreate.
			drops = append(drops, drop(o))
			adds = append(adds, add(c))
		}
	}
	for _, o := range old {
		if !seen[name(o)] {
			x := drop(o)
			x.Destructive = true
			x.Why = kind + " " + name(o) + " on " + t.Name + " is no longer in the model"
			drops = append(drops, x)
		}
	}
	return drops, adds
}

// objectOf marks the changes render returns as working on object.
func objectOf[T any](render func(T) Change, object string) func(T) Change {
	return func(x T) Change {
		c := render(x)
		c.Object = object
		return c
	}
}

func (p *Plan) add(c Change) { p.Changes = append(p.Changes, c) }

func sameFK(a, b *schema.ForeignKey) bool {
	return eq(a.Columns, b.Columns) && a.RefTable == b.RefTable &&
		eq(a.RefColumns, b.RefColumns) && a.OnDelete == b.OnDelete && a.OnUpdate == b.OnUpdate &&
		a.Deferrable == b.Deferrable
}

func findCheck(cs []*schema.Check, name string) *schema.Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func findUnique(us []*schema.Unique, name string) *schema.Unique {
	for _, u := range us {
		if u.Name == name {
			return u
		}
	}
	return nil
}

func findIndex(is []*schema.Index, name string) *schema.Index {
	for _, i := range is {
		if i.Name == name {
			return i
		}
	}
	return nil
}

func findFK(fs []*schema.ForeignKey, name string) *schema.ForeignKey {
	for _, f := range fs {
		if f.Name == name {
			return f
		}
	}
	return nil
}

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

// renames are the table and column renames a plan makes, decided once from
// the model's RenamedFrom and then applied to everything the plan compares.
//
// They are keyed by a table's name in the database the plan starts from. The
// entry under "" is the table's new name, when the table itself is renamed;
// every other entry maps one of its columns' old name to the new one.
type renames map[string]map[string]string

// renamesOf reads the renames `to` declares that `from` still needs: the old
// name exists, the new one does not yet, and the model does not also declare
// the old one. A rename the database already has is no rename, which is what
// lets RenamedFrom stay in a model after every database has moved.
func renamesOf(from, to *schema.Schema) renames {
	r := renames{}
	set := func(table, old, new string) {
		if r[table] == nil {
			r[table] = map[string]string{}
		}
		r[table][old] = new
	}
	for _, t := range to.Tables {
		ot := from.Table(t.Name)
		if ot == nil && t.RenamedFrom != "" && to.Table(t.RenamedFrom) == nil {
			if ot = from.Table(t.RenamedFrom); ot != nil {
				set(ot.Name, "", t.Name)
			}
		}
		if ot == nil {
			continue
		}
		for _, c := range t.Columns {
			if c.RenamedFrom == "" || ot.Column(c.Name) != nil || t.Column(c.RenamedFrom) != nil ||
				ot.Column(c.RenamedFrom) == nil {
				continue
			}
			set(ot.Name, c.RenamedFrom, c.Name)
		}
	}
	return r
}

// invert is the renames of the plan back.
func (r renames) invert() renames {
	out := renames{}
	for old, m := range r {
		n := r.table(old)
		out[n] = map[string]string{}
		for o, c := range m {
			if o == "" {
				out[n][""] = old
			} else {
				out[n][c] = o
			}
		}
	}
	return out
}

// tableOrder is the renamed tables' old names, sorted, so the plan is
// deterministic.
func (r renames) tableOrder() []string {
	var out []string
	for old, m := range r {
		if _, ok := m[""]; ok {
			out = append(out, old)
		}
	}
	slices.Sort(out)
	return out
}

// table is the name the table called old will have.
func (r renames) table(old string) string {
	if n, ok := r[old][""]; ok {
		return n
	}
	return old
}

// oldColumn is the name the column now called c, of the table now called
// table, had before it was renamed.
func (r renames) oldColumn(table, c string) (string, bool) {
	for old, m := range r {
		if r.table(old) != table {
			continue
		}
		for o, n := range m {
			if o != "" && n == c {
				return o, true
			}
		}
	}
	return "", false
}

// apply is s as it stands once every rename has run: the tables and columns
// renamed, and every key, index and foreign key naming them following them,
// as the server makes them follow. Nothing in s is modified.
func (r renames) apply(s *schema.Schema) *schema.Schema {
	if len(r) == 0 {
		return s
	}
	out := *s
	out.Tables = make([]*schema.Table, len(s.Tables))
	for i, t := range s.Tables {
		out.Tables[i] = r.applyTable(t)
	}
	return &out
}

// column is the name column c of the table called table will have.
func (r renames) column(table, c string) string {
	if n, ok := r[table][c]; ok && c != "" {
		return n
	}
	return c
}

func (r renames) applyTable(t *schema.Table) *schema.Table {
	nt := *t
	nt.Name = r.table(t.Name)
	cn := func(c string) string { return r.column(t.Name, c) }
	names := func(cs []string, f func(string) string) []string {
		if cs == nil {
			return nil
		}
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = f(c)
		}
		return out
	}
	nt.Columns = make([]*schema.Column, len(t.Columns))
	for i, c := range t.Columns {
		nc := *c
		nc.Name = cn(c.Name)
		nt.Columns[i] = &nc
	}
	nt.PrimaryKey = names(t.PrimaryKey, cn)
	nt.Uniques = make([]*schema.Unique, len(t.Uniques))
	for i, u := range t.Uniques {
		nu := *u
		nu.Columns = names(u.Columns, cn)
		nt.Uniques[i] = &nu
	}
	nt.Indexes = make([]*schema.Index, len(t.Indexes))
	for i, ix := range t.Indexes {
		ni := *ix
		ni.Columns = slices.Clone(ix.Columns)
		for j := range ni.Columns {
			if !ni.Columns[j].Expr {
				ni.Columns[j].Name = cn(ni.Columns[j].Name)
			}
		}
		ni.Include = names(ix.Include, cn)
		nt.Indexes[i] = &ni
	}
	nt.ForeignKeys = make([]*schema.ForeignKey, len(t.ForeignKeys))
	for i, fk := range t.ForeignKeys {
		nf := *fk
		nf.Columns = names(fk.Columns, cn)
		nf.RefTable = r.table(fk.RefTable)
		nf.RefColumns = names(fk.RefColumns, func(c string) string { return r.column(fk.RefTable, c) })
		nt.ForeignKeys[i] = &nf
	}
	nt.Excludes = make([]*schema.Exclude, len(t.Excludes))
	for i, ex := range t.Excludes {
		ne := *ex
		ne.Parts = slices.Clone(ex.Parts)
		for j := range ne.Parts {
			if !ne.Parts[j].Expr {
				ne.Parts[j].Column = cn(ne.Parts[j].Column)
			}
		}
		nt.Excludes[i] = &ne
	}
	return &nt
}
