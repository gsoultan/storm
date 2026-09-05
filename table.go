package storm

import (
	"fmt"
	"reflect"
	"strings"
	"unsafe"

	"github.com/gsoultan/storm/schema"
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

func (b *ColBuilder) Named(n string) *ColBuilder { b.c.sc.Name = n; return b }
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
