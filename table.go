package storm

import (
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"github.com/gsoultan/storm/schema"
	"time"
)

// Table is the builder handed to a model's Schema method. Every reference to a
// field is a *field pointer* (&u.Email), so a rename is a compile error and a
// typo never compiles.
type Table struct {
	base unsafe.Pointer   // address of the zero model the builder allocated
	typ  reflect.Type     // the model struct type
	out  *schema.Table    // what we are filling in
	off  map[uintptr]*col // field offset -> column being built
	errs *errorList

	// relOff maps a RELATION field's offset to its Go field name. Separate
	// from off because a has-many contributes no column — the key lives on the
	// other table — so `&u.Posts` has nothing in off to resolve to, and a plan
	// still has to be able to name it.
	relOff map[uintptr]string

	// through are the t.Through declarations, resolved after every table's
	// keys exist — the join model's foreign keys are what name the columns.
	through []throughDecl

	// acrossDeleted records the uniqueness declarations that must NOT be
	// scoped to live rows on a soft-delete table — an identifier that may never
	// be reissued even to a deleted row's successor. Keyed by column list for
	// constraints and by pointer for indexes, because that is what each
	// declaration has to identify itself with.
	acrossDeleted   map[string]bool
	acrossDeletedIx map[*schema.Index]bool

	// fks are the explicit t.ForeignKey declarations, held until every table's
	// columns and keys exist — the referenced table is named by TYPE here, and
	// the columns it is keyed on may themselves be declared in its own Schema
	// method, which may not have run yet.
	fks []*fkDecl

	// noAutoIdx are the foreign-key columns that must NOT get storm's
	// automatic index. Keyed by the column being built rather than by name, so
	// a later t.Col(...).Named() cannot strand the entry.
	noAutoIdx map[*col]bool
}

// fkDecl is one t.ForeignKey(...).References(...) call.
type fkDecl struct {
	cols      []string     // this table's columns, resolved at declaration
	targetTyp reflect.Type // the referenced model
	refOffs   []uintptr    // field offsets into the referenced model
	name      string
	onDelete  schema.Action
	onUpdate  schema.Action
	resolved  bool // References was called
	noIndex   bool
}

// throughDecl is one t.Through call, held until the join model's own keys are
// built.
type throughDecl struct {
	field string
	join  reflect.Type
}

// col carries build-time state that does not live in the IR.
type col struct {
	sc    *schema.Column
	field reflect.StructField
	isRel bool
	// anyRef is set on the TYPE column of a discriminator pair, so
	// AcknowledgeNoFK reaches the declaration through the field pointer the
	// model already writes.
	anyRef *schema.AnyRefField
}

// Col addresses a column by field pointer and returns a builder for it.
//
//	t.Col(&u.Email).Unique().Size(320)
func (t *Table) Col(fieldPtr any) *ColBuilder {
	c, err := t.resolve(fieldPtr)
	if err != nil {
		t.errs.add(err)
		return &ColBuilder{t: t, c: &col{sc: &schema.Column{}}} // keep chaining safe
	}
	return &ColBuilder{t: t, c: c}
}

// resolve maps a &field pointer back to the column it names.
func (t *Table) resolve(fieldPtr any) (*col, error) {
	if fieldPtr == nil {
		return nil, fmt.Errorf("%s: nil field pointer", t.out.Name)
	}
	v := reflect.ValueOf(fieldPtr)
	if v.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("%s: Col wants a field pointer like &m.Field, got %T", t.out.Name, fieldPtr)
	}
	if v.IsNil() {
		return nil, fmt.Errorf("%s: nil field pointer", t.out.Name)
	}

	base := uintptr(t.base)
	got := v.Pointer()
	size := t.typ.Size()
	if got < base || got >= base+size {
		return nil, fmt.Errorf(
			"%s: field pointer does not point into the model — Schema must use a POINTER receiver "+
				"(func (m *%s) Schema(t *storm.Table)); a value receiver copies the struct first",
			t.out.Name, t.typ.Name())
	}
	c, ok := t.off[got-base]
	if !ok {
		return nil, fmt.Errorf("%s: no column at field offset %d (unexported or ignored field?)",
			t.out.Name, got-base)
	}
	return c, nil
}

// offsetOf is a field pointer's offset within the model, or ^uintptr(0) when it
// does not point into one. Relations have no column, so resolve cannot find
// them; relOff is keyed by this.
func (t *Table) offsetOf(fieldPtr any) uintptr {
	v := reflect.ValueOf(fieldPtr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ^uintptr(0)
	}
	base, got := uintptr(t.base), v.Pointer()
	if got < base || got >= base+t.typ.Size() {
		return ^uintptr(0)
	}
	return got - base
}

// names resolves a list of field pointers to column names, in order.
func (t *Table) names(ptrs []any) []string {
	out := make([]string, 0, len(ptrs))
	for _, p := range ptrs {
		if ic, ok := p.(IndexColumn); ok {
			out = append(out, ic.name(t))
			continue
		}
		c, err := t.resolve(p)
		if err != nil {
			t.errs.add(err)
			continue
		}
		out = append(out, c.sc.Name)
	}
	return out
}

// ---- table-level constraints ----

// PrimaryKey overrides the inferred key. Pass several field pointers for a
// composite key.
func (t *Table) PrimaryKey(fields ...any) *Table {
	t.out.PrimaryKey = t.names(fields)
	return t
}

// Through declares a many-to-many that runs over a join model the adopter
// wrote, rather than a table storm generated.
//
// The reason to write the join yourself is that it carries columns of its own —
// when a role was granted, when it expires — and those columns are the point.
// A generated join table has nowhere to put them.
//
//	func (u *User) Schema(t *storm.Table) {
//	    t.Through(&u.Roles, UserRole{})
//	}
//
// `join` is a VALUE of the join model, not a pointer and not a field pointer:
// it names a type, and the type is all storm needs. The join model must carry
// exactly one foreign key to each end, which is what makes the two directions
// unambiguous — a join with two keys to the same table is a self-referential
// shape that has to name its own columns.
//
// The plan row carries the join row and the far row together, so the payload
// is reachable: `u.Roles[i].GrantedAt` alongside `u.Roles[i].Role.Name`.
func (t *Table) Through(fieldPtr any, join any) *Table {
	name, ok := t.relOff[t.offsetOf(fieldPtr)]
	if !ok {
		t.errs.add(fmt.Errorf(
			"%s: Through's first argument must be a relation field on this model", t.out.Name))
		return t
	}
	jt := reflect.TypeOf(join)
	for jt != nil && jt.Kind() == reflect.Ptr {
		jt = jt.Elem()
	}
	if jt == nil || jt.Kind() != reflect.Struct {
		t.errs.add(fmt.Errorf(
			"%s.%s: Through needs the join MODEL as a value — t.Through(&u.Roles, UserRole{})",
			t.out.Name, name))
		return t
	}
	t.through = append(t.through, throughDecl{field: name, join: jt})
	return t
}

// Unique adds a table-level uniqueness constraint.
//
// Postgres UNIQUE constraints cannot contain expressions, so a Unique over one
// (Lower(&u.Email)) is emitted as a UNIQUE INDEX instead. Same guarantee,
// different object — and the alternative is DDL that does not parse.
func (t *Table) UniqueNamed(name string, cols ...any) *Table {
	t.Unique(cols...)
	if n := len(t.out.Uniques); n > 0 {
		t.out.Uniques[n-1].Name = name
	}
	return t
}

func (t *Table) Unique(cols ...any) *Table {
	ixc := make([]schema.IndexColumn, 0, len(cols))
	hasExpr := false
	for _, c := range cols {
		k := t.indexCol(c)
		if k.Expr {
			hasExpr = true
		}
		ixc = append(ixc, k)
	}
	if hasExpr {
		t.out.Indexes = append(t.out.Indexes, &schema.Index{Columns: ixc, Unique: true})
		return t
	}
	names := make([]string, len(ixc))
	for i, k := range ixc {
		names[i] = k.Name
	}
	t.out.Uniques = append(t.out.Uniques, &schema.Unique{Columns: names})
	return t
}

// Index adds a secondary index. Wrap a field in Desc(...) or Lower(...) to
// order or transform it.
func (t *Table) Index(cols ...any) *IndexBuilder {
	ix := &schema.Index{}
	for _, c := range cols {
		ix.Columns = append(ix.Columns, t.indexCol(c))
	}
	t.out.Indexes = append(t.out.Indexes, ix)
	return &IndexBuilder{t: t, ix: ix}
}

func (t *Table) indexCol(c any) schema.IndexColumn {
	if ic, ok := c.(IndexColumn); ok {
		return schema.IndexColumn{
			Name: ic.name(t), Expr: ic.expr != "", Desc: ic.desc,
			NullsLast: ic.nullsLast, NullsFirst: ic.nullsFirst,
			OpClass: ic.opClass, Collate: ic.collate, Prefix: ic.prefix,
		}
	}
	col, err := t.resolve(c)
	if err != nil {
		t.errs.add(err)
		return schema.IndexColumn{}
	}
	return schema.IndexColumn{Name: col.sc.Name}
}

// Check adds a CHECK constraint.
func (t *Table) Check(e Expr) *Table {
	t.out.Checks = append(t.out.Checks, &schema.Check{Expr: string(e)})
	return t
}

// CheckNamed is Check with the constraint name pinned. UniqueNamed is Unique
// with the same.
//
// Both exist for adoption, not for new models: PostgreSQL names these
// <table>_<column>_check and <table>_<column>_key, storm derives ck_ and uq_
// prefixes, and a model that cannot say the existing name proposes to drop and
// recreate every constraint in a database it was just imported from. New
// models should use Check and Unique and let storm name them.
func (t *Table) CheckNamed(name string, e Expr) *Table {
	t.out.Checks = append(t.out.Checks, &schema.Check{Name: name, Expr: string(e)})
	return t
}

// Exclude adds an exclusion constraint — the correct answer to booking and
// scheduling overlap, and reachable from no other Go ORM.
//
//	t.Exclude(storm.With(&b.Room, storm.OpEq), storm.With(&b.Period, storm.OpOverlaps))
func (t *Table) Exclude(parts ...ExcludeSpec) *ExcludeBuilder {
	ex := &schema.Exclude{}
	for _, p := range parts {
		if p.expr != "" {
			ex.Parts = append(ex.Parts, schema.ExcludePart{Column: p.expr, Expr: true, Operator: p.op})
			continue
		}
		c, err := t.resolve(p.field)
		if err != nil {
			t.errs.add(err)
			continue
		}
		ex.Parts = append(ex.Parts, schema.ExcludePart{Column: c.sc.Name, Operator: p.op})
	}
	t.out.Excludes = append(t.out.Excludes, ex)
	return &ExcludeBuilder{t: t, ex: ex}
}

// Name overrides the table name inferred from the Go type.
func (t *Table) Name(n string) *Table { t.out.Name = n; return t }

// Comment sets a table comment.
func (t *Table) Comment(s string) *Table { t.out.Comment = s; return t }

// ---- builders ----

// ColBuilder configures one column.
type ColBuilder struct {
	t *Table
	c *col
}

// Named renames the column this field maps to.
//
// A renamed column takes its foreign key with it. The key was built from the
// relation's DERIVED column name — snake(field)+"_id" — before any Schema
// method ran, so renaming the column and not the key leaves the key naming a
// column that no longer exists: the constraint is emitted against the wrong
// name, and every later t.Col(&m.Field).OnDelete on the same field reports
// "not a foreign key", because that is looked up BY column name.
func (b *ColBuilder) Named(n string) *ColBuilder {
	old := b.c.sc.Name
	b.c.sc.Name = n
	if old == n {
		return b
	}
	renameCol(b.t.out, old, n)
	return b
}

// renameCol rewrites every structural reference to a column storm had already
// recorded under its derived name.
//
// A rename is not a property of the column alone: the primary key, the foreign
// keys, the relations and every index and unique constraint name it too, and
// each was populated before any Schema method ran. Fixing them one at a time as
// each was found produced three separate "column does not exist" failures from
// one rename, each in a different kind of DDL; this does the whole rename.
//
// Expressions — CHECK bodies, partial-index WHERE, generated columns — are NOT
// rewritten. They are SQL text storm does not parse, and a textual substitution
// inside one would corrupt a string literal that happens to contain the name.
// A rename under an expression that mentions the old column is a build failure
// against the scratch schema, which is where it belongs.
func renameCol(t *schema.Table, old, n string) {
	for i, c := range t.PrimaryKey {
		if c == old {
			t.PrimaryKey[i] = n
		}
	}
	for _, fk := range t.ForeignKeys {
		for i, c := range fk.Columns {
			if c == old {
				fk.Columns[i] = n
			}
		}
	}
	for _, rel := range t.Relations {
		if rel.Column == old {
			rel.Column = n
		}
	}
	for _, u := range t.Uniques {
		for i, c := range u.Columns {
			if c == old {
				u.Columns[i] = n
			}
		}
	}
	for _, ix := range t.Indexes {
		for i, c := range ix.Columns {
			if c.Name == old {
				ix.Columns[i].Name = n
			}
		}
		for i, c := range ix.Include {
			if c == old {
				ix.Include[i] = n
			}
		}
	}
}

func (b *ColBuilder) Size(n int) *ColBuilder {
	b.c.sc.Type.Name = schema.TypeVarchar
	b.c.sc.Type.Size = n
	return b
}
func (b *ColBuilder) Numeric(p, s int) *ColBuilder {
	b.c.sc.Type.Precision, b.c.sc.Type.Scale = p, s
	return b
}

// Date narrows a time.Time column to a calendar date. The Go type stays
// time.Time (there is no stdlib date), decoded as midnight UTC.
func (b *ColBuilder) Date() *ColBuilder {
	if b.c.sc.Type.Name != schema.TypeTimestamptz {
		b.t.errs.add(fmt.Errorf("%s: .Date() applies to a time.Time field, not %s",
			b.t.out.Name, b.c.sc.Type.Name))
		return b
	}
	b.c.sc.Type.Name = schema.TypeDate
	return b
}

// Cidr narrows a netip.Prefix column from inet to cidr — the database then
// rejects host bits, which is the entire difference between the two types.
func (b *ColBuilder) Cidr() *ColBuilder {
	if b.c.sc.Type.Name != schema.TypeInet {
		b.t.errs.add(fmt.Errorf("%s: .Cidr() applies to a netip.Prefix field, not %s",
			b.t.out.Name, b.c.sc.Type.Name))
		return b
	}
	b.c.sc.Type.Name = schema.TypeCIDR
	return b
}

func (b *ColBuilder) Default(e Expr) *ColBuilder   { b.c.sc.Default = string(e); return b }
func (b *ColBuilder) Generated(e Expr) *ColBuilder { b.c.sc.Generated = string(e); return b }
func (b *ColBuilder) Immutable() *ColBuilder       { b.c.sc.Immutable = true; return b }
func (b *ColBuilder) Version() *ColBuilder         { b.c.sc.Version = true; return b }
func (b *ColBuilder) Comment(s string) *ColBuilder { b.c.sc.Comment = s; return b }
func (b *ColBuilder) NotNull() *ColBuilder         { b.c.sc.NotNull = true; return b }
func (b *ColBuilder) Nullable() *ColBuilder        { b.c.sc.NotNull = false; return b }

// AcknowledgeNoFK records why this AnyRef gives up referential integrity.
//
// Required: Build refuses an AnyRef without one. The reason travels into the
// schema and out through `storm diff`, so the decision is visible where it is
// reviewed rather than only where it was made.
func (b *ColBuilder) AcknowledgeNoFK(reason string) *ColBuilder {
	switch {
	case b.c.anyRef == nil:
		b.t.errs.add(fmt.Errorf(
			"%s.%s: AcknowledgeNoFK is only meaningful on a storm.AnyRef field — "+
				"an ordinary reference already has a foreign key",
			b.t.out.Name, b.c.sc.Name))
	case strings.TrimSpace(reason) == "":
		b.t.errs.add(fmt.Errorf(
			"%s.%s: AcknowledgeNoFK needs a reason — it is what a reviewer reads in the diff",
			b.t.out.Name, b.c.anyRef.Field))
	default:
		b.c.anyRef.Reason = reason
	}
	return b
}

// Serial makes the column smallserial, serial or bigserial, taking the width
// from the Go type — int16, int32, int64.
//
// It is here to describe databases that already exist. For a NEW model prefer
// Identity: it is the SQL-standard form, it is what every other dialect storm
// targets can express, and a serial's sequence is a separate object whose
// permissions and ownership are one more thing to get right. storm keeps them
// apart rather than normalising one to the other, because a model that says
// identity about a database holding a serial proposes an ALTER on the first
// diff — a change nobody asked for on the first day of adoption.
func (b *ColBuilder) Serial() *ColBuilder {
	switch b.c.sc.Type.Name {
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
	default:
		b.t.errs.add(fmt.Errorf("%s.%s: Serial on a %s column — serial is an integer sequence, "+
			"so the field must be an int16, int32 or int64",
			b.t.out.Name, b.c.sc.Name, b.c.sc.Type.SQL()))
		return b
	}
	b.c.sc.Serial = true
	return b
}

// Identity makes the column GENERATED BY DEFAULT AS IDENTITY.
//
// The IR and the DDL for this have existed since the schema package did; the
// builder had not, so no model could say it. See Serial for which to reach for.
func (b *ColBuilder) Identity() *ColBuilder {
	switch b.c.sc.Type.Name {
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
	default:
		b.t.errs.add(fmt.Errorf("%s.%s: Identity on a %s column — an identity is an integer "+
			"sequence, so the field must be an int16, int32 or int64",
			b.t.out.Name, b.c.sc.Name, b.c.sc.Type.SQL()))
		return b
	}
	b.c.sc.Identity = true
	return b
}

// Raw forces a database type storm does not model.
func (b *ColBuilder) Raw(sqlType string) *ColBuilder {
	b.c.sc.Type = schema.Type{Name: sqlType}
	return b
}

// Unique adds a single-column unique constraint. On a foreign key this is what
// turns one-to-many into one-to-one.
func (b *ColBuilder) Unique() *ColBuilder {
	b.t.out.Uniques = append(b.t.out.Uniques, &schema.Unique{Columns: []string{b.c.sc.Name}})
	return b
}

// Index adds a single-column index.
func (b *ColBuilder) Index() *ColBuilder {
	b.t.out.Indexes = append(b.t.out.Indexes,
		&schema.Index{Columns: []schema.IndexColumn{{Name: b.c.sc.Name}}})
	return b
}

// OnDelete and OnUpdate set the referential action of this column's foreign key.
func (b *ColBuilder) OnDelete(a Action) *ColBuilder { return b.setAction(a, true) }
func (b *ColBuilder) OnUpdate(a Action) *ColBuilder { return b.setAction(a, false) }

// ConstraintName pins the foreign key's constraint name.
//
// storm otherwise derives one (fk_<table>_<column>), which is right for a
// schema storm created and wrong for one it is adopting: PostgreSQL's own
// default is <table>_<column>_fkey, so importing an existing database produced
// a model whose first migration DROPped and re-ADDed every foreign key in it.
// That is a lock on a large table and a change of the name an application may
// be matching in its error handling, to gain nothing.
func (b *ColBuilder) ConstraintName(n string) *ColBuilder {
	fk := b.t.fkFor(b.c.sc.Name)
	if fk == nil {
		b.t.errs.add(fmt.Errorf("%s.%s: ConstraintName on a column that is not a foreign key",
			b.t.out.Name, b.c.sc.Name))
		return b
	}
	fk.Name = n
	return b
}

func (b *ColBuilder) setAction(a Action, del bool) *ColBuilder {
	fk := b.t.fkFor(b.c.sc.Name)
	if fk == nil {
		b.t.errs.add(fmt.Errorf("%s.%s: OnDelete/OnUpdate on a column that is not a foreign key",
			b.t.out.Name, b.c.sc.Name))
		return b
	}
	if del {
		if a == SetNull && b.c.sc.NotNull {
			b.t.errs.add(fmt.Errorf(
				"%s.%s: OnDelete(SetNull) on a NOT NULL column — the action could never fire; make the relation a pointer",
				b.t.out.Name, b.c.sc.Name))
			return b
		}
		fk.OnDelete = schema.Action(a)
	} else {
		fk.OnUpdate = schema.Action(a)
	}
	return b
}

func (t *Table) fkFor(colName string) *schema.ForeignKey {
	for _, fk := range t.out.ForeignKeys {
		if len(fk.Columns) == 1 && fk.Columns[0] == colName {
			return fk
		}
	}
	return nil
}

// IndexBuilder configures an index after Index(...).
type IndexBuilder struct {
	t  *Table
	ix *schema.Index
}

// Using selects the access method: storm.BTree (the default), storm.Hash,
// storm.GIN, storm.GiST, storm.SPGiST, storm.BRIN — or, for a MySQL target,
// storm.FullText. A method the target lacks fails generation naming both.
func (b *IndexBuilder) Using(method string) *IndexBuilder { b.ix.Method = method; return b }
func (b *IndexBuilder) Unique() *IndexBuilder             { b.ix.Unique = true; return b }
func (b *IndexBuilder) Where(e Expr) *IndexBuilder        { b.ix.Where = string(e); return b }
func (b *IndexBuilder) Named(n string) *IndexBuilder      { b.ix.Name = n; return b }

// Include adds non-key columns to the index's leaf entries, so a read that
// touches only the key and these is answered from the index alone — the
// covering index, and an Index Only Scan with zero heap fetches. They take
// part in no ordering and no uniqueness check.
//
//	t.Index(&o.Customer, storm.Desc(&o.PlacedAt)).Include(&o.Status, &o.Total)
//
// btree, gist and spgist carry them; gin, hash and brin cannot.
func (b *IndexBuilder) Include(fields ...any) *IndexBuilder {
	for _, f := range fields {
		col, err := b.t.resolve(f)
		if err != nil {
			b.t.errs.add(err)
			continue
		}
		b.ix.Include = append(b.ix.Include, col.sc.Name)
	}
	return b
}

// NullsNotDistinct makes a unique index treat NULLs as equal, so at most one
// row may leave the key NULL (PostgreSQL 15+). SQL's default is that every
// NULL is distinct from every other, which lets a "unique" nullable column
// hold any number of them.
func (b *IndexBuilder) NullsNotDistinct() *IndexBuilder { b.ix.NullsNotDistinct = true; return b }

// With sets a storage parameter: fillfactor on a btree that takes updates in
// place, fastupdate on a gin, pages_per_range on a brin. Which parameters a
// method accepts is checked at build time.
func (b *IndexBuilder) With(name, value string) *IndexBuilder {
	b.ix.With = append(b.ix.With, schema.StorageParam{Name: name, Value: value})
	return b
}

// Invisible hides the index from the planner while keeping it maintained
// (MySQL 8.0+): the safe way to drop an index is to hide it first and watch
// the plans. PostgreSQL has no equivalent and refuses the declaration.
func (b *IndexBuilder) Invisible() *IndexBuilder { b.ix.Invisible = true; return b }

// ExcludeBuilder configures an exclusion constraint.
type ExcludeBuilder struct {
	t  *Table
	ex *schema.Exclude
}

func (b *ExcludeBuilder) Using(method string) *ExcludeBuilder { b.ex.Method = method; return b }
func (b *ExcludeBuilder) Where(e Expr) *ExcludeBuilder        { b.ex.Where = string(e); return b }
func (b *ExcludeBuilder) Named(n string) *ExcludeBuilder      { b.ex.Name = n; return b }

// ---- index column modifiers ----

// IndexColumn is a field reference with ordering or an expression applied.
type IndexColumn struct {
	field      any
	expr       string // "%s" is replaced by the column name
	desc       bool
	nullsLast  bool
	nullsFirst bool
	opClass    string
	collate    string
	prefix     int
}

// asKey accepts either a bare field pointer or a key that already has
// modifiers, so the modifiers compose in any order:
// storm.OpClass(storm.Lower(&u.Email), "text_pattern_ops").
func asKey(key any) IndexColumn {
	if ic, ok := key.(IndexColumn); ok {
		return ic
	}
	return IndexColumn{field: key}
}

func (ic IndexColumn) name(t *Table) string {
	if ic.field == nil {
		return ic.expr
	}
	c, err := t.resolve(ic.field)
	if err != nil {
		t.errs.add(err)
		return ""
	}
	if ic.expr == "" {
		return c.sc.Name
	}
	return stripOuterParens(fmt.Sprintf(ic.expr, c.sc.Name))
}

// stripOuterParens removes one enclosing pair of parentheses from an
// expression key. The emitter adds its own, and PostgreSQL prints the
// expression back without any it did not need — so "(score + 1)" declared
// here would read back as "score + 1", compare unequal, and be dropped and
// recreated on every diff.
func stripOuterParens(s string) string {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s
			}
		}
	}
	return s[1 : len(s)-1]
}

// Desc orders an index key descending.
func Desc(field any) IndexColumn { return IndexColumn{field: field, desc: true} }

// Asc is the default; provided for symmetry.
func Asc(field any) IndexColumn { return IndexColumn{field: field} }

// NullsLast puts NULLs at the end of a DESCENDING key, where they would
// otherwise come first. On an ascending key it is already the default, and
// the declaration is refused rather than recorded as a fact the database
// will not remember.
func NullsLast(key any) IndexColumn { ic := asKey(key); ic.nullsLast = true; return ic }

// NullsFirst puts NULLs before every value on an ASCENDING key. A descending
// key already does, and saying so is refused for the same reason as above.
func NullsFirst(key any) IndexColumn { ic := asKey(key); ic.nullsFirst = true; return ic }

// OpClass sets the key's operator class — what makes an index answer a
// question its default class cannot:
//
//	storm.OpClass(&u.Email, "text_pattern_ops")   // LIKE 'abc%' under any collation
//	storm.OpClass(&u.Prefs, "jsonb_path_ops")     // a smaller gin that answers only @>
//	storm.OpClass(&u.Name, "gin_trgm_ops")        // ILIKE '%abc%' — installs pg_trgm
//
// Which classes a method accepts is checked at build time for the classes
// storm knows; an unknown one is passed through for the server to judge.
func OpClass(key any, class string) IndexColumn { ic := asKey(key); ic.opClass = class; return ic }

// Collate overrides the column's collation for this key. "C" is the one that
// matters: under it a plain btree serves a prefix LIKE without an opclass.
func Collate(key any, collation string) IndexColumn {
	ic := asKey(key)
	ic.collate = collation
	return ic
}

// Prefix indexes only the first n characters of a text or binary column —
// the only way MySQL can index a TEXT or BLOB column at all, and refused for
// PostgreSQL, which has no prefix index.
func Prefix(field any, n int) IndexColumn { ic := asKey(field); ic.prefix = n; return ic }

// Lower indexes lower(col) — the usual answer for case-insensitive uniqueness.
func Lower(field any) IndexColumn { return IndexColumn{field: field, expr: "lower(%s)"} }

// Upper indexes upper(col).
func Upper(field any) IndexColumn { return IndexColumn{field: field, expr: "upper(%s)"} }

// IndexExpr indexes an expression over the column, with %s standing for the
// column name — date_trunc('day', %s) for a daily report's key, left(%s, 8)
// for a prefix, (%s->>'country') for a jsonb attribute. The expression has to
// be IMMUTABLE, which is PostgreSQL's rule and not storm's: the server refuses
// the index otherwise, at apply time, naming the function.
func IndexExpr(field any, expr string) IndexColumn { return IndexColumn{field: field, expr: expr} }

// ---- exclusion operators ----

// ExcludeSpec is one `<column-or-expression> WITH <operator>` part.
type ExcludeSpec struct {
	field any
	expr  string
	op    string
}

// Exclusion operators.
const (
	OpEq       = "="
	OpOverlaps = "&&"
	OpAdjacent = "-|-"
)

// With pairs a field with an exclusion operator.
func With(field any, op string) ExcludeSpec { return ExcludeSpec{field: field, op: op} }

// WithExpr pairs an expression with an exclusion operator, for the range
// overlap that scalar columns cannot express:
//
//	t.Exclude(storm.With(&b.Room, storm.OpEq),
//	          storm.WithExpr("tstzrange(starts_at, ends_at)", storm.OpOverlaps))
func WithExpr(e RawSQL, op string) ExcludeSpec { return ExcludeSpec{expr: string(e), op: op} }

// SoftDelete makes this table delete rows by marking them, not by removing
// them. The field must be a nullable timestamp (`*time.Time`): NULL is alive,
// non-NULL is the moment it was deleted.
//
//	func (u *User) Schema(t *storm.Table) {
//		t.SoftDelete(&u.DeletedAt)
//	}
//
// It is opt-in and per-table on purpose. Soft delete BY DEFAULT is on storm's
// rejected list (docs/CONCEPT.md) because in a runtime ORM every read that
// forgets the predicate returns rows the application believes are gone, and
// "remember the predicate" is not a property you can hold across a codebase.
//
// A compiler does not have to ask you to remember. The predicate is compiled
// into every read of this table and ANDed AHEAD of the caller's own, so a call
// site can narrow what it sees and cannot widen it. Reaching the deleted rows
// takes a different, and visibly different, function.
//
// The second hazard is the one that bites later: a marked row still occupies
// its unique key, so `t.Unique(&u.Email)` would refuse a new user the address
// of a deleted one. PostgreSQL cannot express "unique among live rows" as a
// constraint — only as a partial unique index — so Build REFUSES a plain
// Unique on a soft-delete table and names the replacement:
//
//	t.Index(&u.Email).Unique().Where("deleted_at IS NULL")
func (t *Table) SoftDelete(fieldPtr any) *Table {
	c, err := t.resolve(fieldPtr)
	if err != nil {
		t.errs.add(err)
		return t
	}
	if c.field.Type != reflect.PointerTo(reflect.TypeOf(time.Time{})) {
		t.errs.add(fmt.Errorf(
			"%s.%s: SoftDelete wants a *time.Time — NULL is the row that is alive, "+
				"and a non-nullable column has no way to say so (got %s)",
			t.out.Name, c.field.Name, c.field.Type))
		return t
	}
	if t.out.SoftDelete != "" && t.out.SoftDelete != c.sc.Name {
		t.errs.add(fmt.Errorf("%s: SoftDelete declared twice, on %s and %s",
			t.out.Name, t.out.SoftDelete, c.sc.Name))
		return t
	}
	t.out.SoftDelete = c.sc.Name
	return t
}

// UniqueAcrossDeleted declares uniqueness that a soft-delete table's marked
// rows still take part in.
//
// On a soft-delete table an ordinary t.Unique is scoped to the live rows, so a
// deleted row and a new one may hold the same value — which is what makes soft
// delete usable. Sometimes the other reading is the right one: an external
// identifier that must never be reissued, a slug reserved permanently the first
// time it is used, an audit key. Those say so here, and the declaration is
// emitted as a real UNIQUE constraint covering every row, deleted or not.
//
// On a table that does not soft-delete this is exactly t.Unique.
func (t *Table) UniqueAcrossDeleted(cols ...any) *Table {
	t.Unique(cols...)
	if n := len(t.out.Uniques); n > 0 {
		if t.acrossDeleted == nil {
			t.acrossDeleted = map[string]bool{}
		}
		t.acrossDeleted[uniqueKey(t.out.Uniques[n-1].Columns)] = true
	}
	// An expression unique lands in Indexes rather than Uniques.
	if n := len(t.out.Indexes); n > 0 && t.out.Indexes[n-1].Unique {
		if t.acrossDeletedIx == nil {
			t.acrossDeletedIx = map[*schema.Index]bool{}
		}
		t.acrossDeletedIx[t.out.Indexes[n-1]] = true
	}
	return t
}

// AcrossDeleted keeps this unique index covering the marked rows too, instead
// of being scoped to the live ones. See UniqueAcrossDeleted.
func (b *IndexBuilder) AcrossDeleted() *IndexBuilder {
	if b.t.acrossDeletedIx == nil {
		b.t.acrossDeletedIx = map[*schema.Index]bool{}
	}
	b.t.acrossDeletedIx[b.ix] = true
	return b
}

// ForeignKey declares a foreign key over one or more columns.
//
// A single-column key is usually better said with a relation field — `Tenant
// Tenant` builds the column, the key and the index together. This is for the
// keys a relation field cannot express, and the common one is the COMPOSITE
// key that carries a tenant:
//
//	func (g *Grant) Schema(t *storm.Table) {
//	    var i Identity
//	    t.ForeignKey(&g.IdentityID, &g.TenantID).
//	        References(&i, &i.ID, &i.TenantID).
//	        OnDelete(storm.Cascade)
//	}
//
// That key is not a stylistic choice. A single-column key to identities(id)
// lets a row in tenant A reference a parent in tenant B, and the database will
// hold the line only if the tenant travels IN the key. Every such constraint a
// model cannot express is one the schema silently loses.
func (t *Table) ForeignKey(fields ...any) *FKBuilder {
	d := &fkDecl{cols: t.names(fields)}
	t.fks = append(t.fks, d)
	return &FKBuilder{t: t, d: d}
}

// FKBuilder configures a foreign key after ForeignKey(...).
type FKBuilder struct {
	t *Table
	d *fkDecl
}

// References names the table and columns the key points at.
//
// The target is a LOCAL VARIABLE of the referenced model and the columns are
// field pointers into it, the same way a join names a column on another table:
//
//	var i Identity
//	t.ForeignKey(&g.IdentityID, &g.TenantID).References(&i, &i.ID, &i.TenantID)
//
// Field pointers rather than column-name strings so the editor completes them
// and a rename on the far side reaches this declaration instead of leaving a
// string that still compiles and no longer resolves.
func (b *FKBuilder) References(target any, cols ...any) *FKBuilder {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		b.t.errs.add(fmt.Errorf(
			"%s: References wants a pointer to a local model variable — "+
				"var i Identity; t.ForeignKey(...).References(&i, &i.ID), got %T",
			b.t.out.Name, target))
		return b
	}
	if len(cols) != len(b.d.cols) {
		b.t.errs.add(fmt.Errorf(
			"%s: foreign key on (%s) references %d column(s); the two sides must match",
			b.t.out.Name, strings.Join(b.d.cols, ", "), len(cols)))
		return b
	}
	base := v.Pointer()
	size := v.Elem().Type().Size()
	offs := make([]uintptr, 0, len(cols))
	for i, c := range cols {
		cv := reflect.ValueOf(c)
		if cv.Kind() != reflect.Pointer || cv.IsNil() {
			b.t.errs.add(fmt.Errorf(
				"%s: References argument %d wants a field pointer like &i.ID, got %T",
				b.t.out.Name, i+1, c))
			return b
		}
		got := cv.Pointer()
		if got < base || got >= base+size {
			b.t.errs.add(fmt.Errorf(
				"%s: References argument %d does not point into the model passed as the target — "+
					"take the pointers from the same local variable",
				b.t.out.Name, i+1))
			return b
		}
		offs = append(offs, got-base)
	}
	b.d.targetTyp = v.Elem().Type()
	b.d.refOffs = offs
	b.d.resolved = true
	return b
}

// Named sets the constraint name. Without it storm generates one.
func (b *FKBuilder) Named(n string) *FKBuilder { b.d.name = n; return b }

// OnDelete sets the referential action for a deleted parent.
func (b *FKBuilder) OnDelete(a Action) *FKBuilder { b.d.onDelete = schema.Action(a); return b }

// OnUpdate sets the referential action for an updated parent key.
func (b *FKBuilder) OnUpdate(a Action) *FKBuilder { b.d.onUpdate = schema.Action(a); return b }

// NoIndex suppresses the index storm creates for this foreign key.
//
// storm indexes every foreign key by default, and that default is right far
// more often than not: without an index on the child's key, deleting a parent
// scans the whole child table, and so does every ON DELETE CASCADE behind it.
//
// It is a default rather than a rule because it is not always right — a small
// lookup table, a column already covered by a wider index storm cannot see as
// covering, or a write-heavy table where the index costs more than the delete
// it would save. Saying so here is also how a model describes a database that
// already made that choice; without it, storm could not represent such a
// schema at all, and every diff proposed the index again.
func (b *ColBuilder) NoIndex() *ColBuilder {
	if b.t.noAutoIdx == nil {
		b.t.noAutoIdx = map[*col]bool{}
	}
	b.t.noAutoIdx[b.c] = true
	return b
}

// NoIndex suppresses the index storm creates for this foreign key. See
// ColBuilder.NoIndex for when that is the right call.
func (b *FKBuilder) NoIndex() *FKBuilder { b.d.noIndex = true; return b }

// Partition strategies for PartitionBy.
const (
	RangePartition = "RANGE"
	ListPartition  = "LIST"
	HashPartition  = "HASH"
)

// PartitionBy declares the table PARTITIONED BY the given columns.
//
//	func (a *AuditLog) Schema(t *storm.Table) {
//	    t.PartitionBy(storm.RangePartition, &a.OccurredAt)
//	}
//
// The partitions themselves are NOT declared here, and storm neither creates
// nor drops them. That is deliberate: partitions are usually made by a
// scheduled job, so they exist in the database and in no model — and a tool
// that treated "absent from the model" as "delete this" would propose dropping
// last month's rows every time it ran. storm owns the parent; the partitions
// are data.
//
// PostgreSQL requires every PRIMARY KEY and UNIQUE on a partitioned table to
// contain the partition key. storm does not quietly widen one to comply: which
// column belongs in a key is a statement about identity, and the CREATE fails
// naming the table instead.
func (t *Table) PartitionBy(strategy string, fields ...any) *Table {
	switch strategy {
	case RangePartition, ListPartition, HashPartition:
	default:
		t.errs.add(fmt.Errorf("%s: unknown partition strategy %q — use storm.RangePartition, "+
			"storm.ListPartition or storm.HashPartition", t.out.Name, strategy))
		return t
	}
	cols := t.names(fields)
	if len(cols) == 0 {
		t.errs.add(fmt.Errorf("%s: PartitionBy needs at least one column", t.out.Name))
		return t
	}
	t.out.Partition = &schema.Partition{Strategy: strategy, Columns: cols}
	return t
}
