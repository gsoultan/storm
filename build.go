package raorm

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/gsoultan/raorm/runtime"
	"github.com/gsoultan/raorm/schema"
)

var (
	timeType     = reflect.TypeOf(time.Time{})
	intervalType = reflect.TypeOf(runtime.Interval{})
	prefixType   = reflect.TypeOf(netip.Prefix{})
	uuidType     = reflect.TypeOf(UUID{})
	bytesType    = reflect.TypeOf([]byte(nil))
	decimalType  = reflect.TypeOf(Decimal{})
	enumerType   = reflect.TypeOf((*Enumer)(nil)).Elem()
	schemerTyp   = reflect.TypeOf((*Schemer)(nil)).Elem()
)

// Build turns a set of model structs into a schema. Pass pointers to zero
// values: Build(&User{}, &Org{}, &Post{}).
//
// Every declaration problem is collected and reported together, because
// failing on the first one would mean N build cycles to find N mistakes.
func Build(models ...any) (*schema.Schema, error) {
	b := &builder{
		errs:   &errorList{},
		byType: map[reflect.Type]*modelInfo{},
		enums:  map[string]*schema.Enum{},
		outSch: &schema.Schema{},
	}

	// Pass 1: register every model so a field whose type is a model can be
	// told apart from a field whose type is just a struct (which becomes jsonb).
	for _, m := range models {
		if err := b.register(m); err != nil {
			b.errs.add(err)
		}
	}
	// Pass 2: columns and primary keys for every table.
	for _, mi := range b.ordered {
		b.buildColumns(mi)
	}
	// Pass 3: foreign keys, once every table's primary key is known.
	for _, mi := range b.ordered {
		b.resolveRelations(mi)
	}
	// Pass 3b: validate has-many only after every table's keys exist —
	// the key that answers orgs.Users lives on users, built later than orgs.
	for _, mi := range b.ordered {
		b.validateHasMany(mi)
	}
	// Pass 4: user declarations last, so OnDelete has a foreign key to attach
	// to and an explicit setting always wins over an inferred one.
	for _, mi := range b.ordered {
		b.callSchemas(mi)
	}
	// Pass 4b': projections ride the same window as plans, for the same
	// reasons.
	for _, mi := range b.ordered {
		if pr, ok := mi.ptr.Interface().(Projector); ok {
			pr.Projections(&Projections{t: mi.tbl, out: &mi.tbl.out.Projections})
		}
	}
	// Pass 4b: fetch plans, after Schema so a plan names relations whose
	// declarations are final, and after pass 3b so a has-many named by a plan
	// has already been validated to have a key on the other side.
	for _, mi := range b.ordered {
		b.callPlans(mi)
	}
	// Pass 5: index every foreign key nothing already covers. Last, so a user
	// index leading with the same column suppresses the redundant one.
	for _, mi := range b.ordered {
		for _, c := range mi.fkCols {
			if !indexedFirst(mi.tbl.out, c) {
				mi.tbl.out.Indexes = append(mi.tbl.out.Indexes,
					&schema.Index{Columns: []schema.IndexColumn{{Name: c}}})
			}
		}
	}

	if err := b.errs.err(); err != nil {
		return nil, err
	}
	b.outSch.Normalize()
	return b.outSch, nil
}

type builder struct {
	errs    *errorList
	byType  map[reflect.Type]*modelInfo
	ordered []*modelInfo
	enums   map[string]*schema.Enum
	outSch  *schema.Schema
}

type modelInfo struct {
	typ     reflect.Type  // the struct type
	ptr     reflect.Value // pointer to an allocated zero value
	tbl     *Table
	pending []*relation
	fkCols  []string
	arcCols []arcCol
}

// relation is a to-one or to-many link discovered during the column walk and
// resolved once every table's primary key exists.
type relation struct {
	fieldName string
	target    reflect.Type
	toMany    bool
	nullable  bool
	inverse   bool // the other side owns the key
	colName   string
	col       *col
}

func (b *builder) register(m any) error {
	v := reflect.ValueOf(m)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("Build wants a pointer to a zero model, got %T", m)
	}
	t := v.Elem().Type()
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("Build wants a struct model, got %s", t)
	}
	if _, dup := b.byType[t]; dup {
		return fmt.Errorf("%s registered twice", t.Name())
	}
	mi := &modelInfo{typ: t, ptr: v}
	b.byType[t] = mi
	b.ordered = append(b.ordered, mi)
	return nil
}

func (b *builder) buildColumns(mi *modelInfo) {
	st := &schema.Table{Name: tableName(mi.typ.Name()), GoName: mi.typ.Name()}
	tbl := &Table{
		base:   unsafe.Pointer(mi.ptr.Pointer()),
		typ:    mi.typ,
		out:    st,
		off:    map[uintptr]*col{},
		relOff: map[uintptr]string{},
		errs:   b.errs,
	}
	mi.tbl = tbl
	b.outSch.Tables = append(b.outSch.Tables, st)

	b.walk(mi, mi.typ, 0, tbl)

	// Primary key: an `id` column unless the model says otherwise.
	if st.PrimaryKey == nil {
		if st.Column("id") != nil {
			st.PrimaryKey = []string{"id"}
		}
	}
}

// callSchemas runs mixin Schema methods first, then the model's own, so an
// explicit declaration always overrides what a mixin set.
func (b *builder) callSchemas(mi *modelInfo) {
	b.callMixinSchemas(mi, mi.typ, mi.tbl)

	// A value type implements Schemer only when the receiver is a value; a
	// pointer receiver puts the method on *T alone. So this test alone is the
	// detector — adding "&& !PointerTo(T).Implements(...)" would be dead, since
	// *T always implements whatever T does.
	if mi.typ.Implements(schemerTyp) {
		b.errs.add(fmt.Errorf(
			"%s: Schema has a VALUE receiver — it must be a pointer receiver "+
				"(func (m *%s) Schema(t *raorm.Table)), or field pointers cannot be resolved",
			mi.typ.Name(), mi.typ.Name()))
		return
	}
	if s, ok := mi.ptr.Interface().(Schemer); ok {
		s.Schema(mi.tbl)
	}
}

// callMixinSchemas runs the Schema method of each embedded struct against the
// same table, so `Timestamps` can carry its own defaults.
func (b *builder) callMixinSchemas(mi *modelInfo, t reflect.Type, tbl *Table) {
	elem := mi.ptr.Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous || f.Type.Kind() != reflect.Struct {
			continue
		}
		if _, isModel := b.byType[f.Type]; isModel {
			continue
		}
		if s, ok := elem.Field(i).Addr().Interface().(Schemer); ok {
			s.Schema(tbl)
		}
	}
}

// walk flattens a struct into columns, recursing through embedded mixins.
func (b *builder) walk(mi *modelInfo, t reflect.Type, base uintptr, tbl *Table) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		off := base + f.Offset

		// Embedded non-model struct: a mixin, flattened inline.
		if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Type != timeType {
			if _, isModel := b.byType[f.Type]; !isModel {
				b.walk(mi, f.Type, off, tbl)
				continue
			}
		}

		// Exclusive arc? One nullable foreign key per variant.
		if vs, opt, ok := arcVariants(f.Type); ok {
			b.buildArc(mi, tbl, f, off, vs, opt)
			continue
		}

		// Relation?
		if rel := b.relationFor(f); rel != nil {
			// Every relation field is addressable by a plan, whether or not it
			// carries a column.
			tbl.relOff[off] = f.Name
			if rel.toMany {
				// Has-many: the foreign key lives on the other table, so no
				// column here. Recorded so the inverse can be validated.
				mi.pending = append(mi.pending, rel)
				continue
			}
			ownsKey, err := b.owns(mi.typ, rel)
			if err != nil {
				b.errs.add(err)
				continue
			}
			if !ownsKey {
				// Inverse side of a one-to-one: the key lives on the other
				// table, so this field contributes no column.
				rel.inverse = true
				mi.pending = append(mi.pending, rel)
				continue
			}
			rel.colName = fkColumnName(f.Name)
			c := &col{
				sc:    &schema.Column{Name: rel.colName, Type: schema.Type{Name: schema.TypeUUID}, NotNull: !rel.nullable},
				field: f,
				isRel: true,
			}
			tbl.out.Columns = append(tbl.out.Columns, c.sc)
			tbl.off[off] = c
			rel.col = c
			mi.pending = append(mi.pending, rel)
			continue
		}

		// A field that is obviously a model but was not registered would
		// otherwise become a jsonb blob.
		if looksLikeModel(f.Type) || (f.Type.Kind() == reflect.Slice && looksLikeModel(f.Type.Elem())) {
			b.errs.add(fmt.Errorf("%s.%s: %s embeds raorm.Model but is not registered — pass it to Build",
				tbl.out.Name, f.Name, baseName(f.Type)))
			continue
		}

		// Plain column.
		ct, ok := b.inferType(f.Type)
		if !ok {
			b.errs.add(fmt.Errorf("%s.%s: unsupported type %s — give it an explicit type with t.Col(&m.%s).Raw(\"...\")",
				tbl.out.Name, f.Name, f.Type, f.Name))
			continue
		}
		c := &col{
			sc:    &schema.Column{Name: columnName(f.Name), Type: ct, NotNull: !isNullable(f.Type)},
			field: f,
		}
		tbl.out.Columns = append(tbl.out.Columns, c.sc)
		tbl.off[off] = c
	}
}

// relationFor reports whether a field links to another model.
func (b *builder) relationFor(f reflect.StructField) *relation {
	t := f.Type
	switch t.Kind() {
	case reflect.Slice:
		if t == bytesType {
			return nil
		}
		el := t.Elem()
		if el.Kind() == reflect.Pointer {
			el = el.Elem()
		}
		if _, ok := b.byType[el]; ok {
			return &relation{fieldName: f.Name, target: el, toMany: true}
		}
	case reflect.Pointer:
		if _, ok := b.byType[t.Elem()]; ok {
			return &relation{fieldName: f.Name, target: t.Elem(), nullable: true}
		}
	case reflect.Struct:
		if _, ok := b.byType[t]; ok {
			return &relation{fieldName: f.Name, target: t}
		}
	}
	return nil
}

// looksLikeModel reports whether a struct embeds raorm.Model. Such a field was
// meant to be a relation; without this it would silently become a jsonb column,
// which is the worst possible failure mode — it compiles, applies, and is wrong.
func looksLikeModel(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.Anonymous && f.Type == reflect.TypeOf(Model{}) {
			return true
		}
	}
	return false
}

// owns decides which side of a to-one pair carries the foreign key.
//
// The rule is "the required side owns the key", which is both deterministic and
// semantically right: a Profile requires a User, a User may or may not have a
// Profile. Without it, User.Profile *Profile and Profile.User User would each
// emit a key and produce a circular pair of foreign keys.
func (b *builder) owns(self reflect.Type, rel *relation) (bool, error) {
	if rel.target == self {
		return true, nil // self-reference: there is no other side
	}
	tgt := b.byType[rel.target]
	if tgt == nil {
		return true, nil // unresolved; reported elsewhere
	}
	back, backNullable, found := toOneFieldOf(tgt.typ, self)
	if !found {
		return true, nil // nothing points back: we own it
	}
	switch {
	case rel.nullable && !backNullable:
		return false, nil // they are required, they own it
	case !rel.nullable && backNullable:
		return true, nil
	default:
		return false, fmt.Errorf(
			"%s.%s and %s.%s both point at each other and are equally optional — "+
				"make exactly one side a pointer so the required side owns the foreign key",
			snake(self.Name()), rel.fieldName, snake(rel.target.Name()), back)
	}
}

// toOneFieldOf reports whether t has a to-one field of type want.
func toOneFieldOf(t, want reflect.Type) (name string, nullable, found bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Struct:
			if f.Type == want {
				return f.Name, false, true
			}
		case reflect.Pointer:
			if f.Type.Elem() == want {
				return f.Name, true, true
			}
		}
	}
	return "", false, false
}

// resolveRelations points each foreign key at its target's primary key.
func (b *builder) resolveRelations(mi *modelInfo) {
	for _, rel := range mi.pending {
		if rel.inverse {
			continue
		}
		tgt := b.byType[rel.target]
		if tgt == nil || tgt.tbl == nil {
			b.errs.add(fmt.Errorf("%s.%s: %s is not registered — pass it to Build",
				mi.tbl.out.Name, rel.fieldName, rel.target.Name()))
			continue
		}
		if rel.toMany {
			continue // validated in pass 3b
		}
		pk := tgt.tbl.out.PrimaryKey
		if len(pk) != 1 {
			b.errs.add(fmt.Errorf("%s.%s: %s has a %d-column primary key; declare the foreign key explicitly",
				mi.tbl.out.Name, rel.fieldName, tgt.tbl.out.Name, len(pk)))
			continue
		}
		// Adopt the referenced key's type so uuid/bigserial both work.
		if refCol := tgt.tbl.out.Column(pk[0]); refCol != nil && rel.col != nil {
			rel.col.sc.Type = refCol.Type
		}
		mi.tbl.out.ForeignKeys = append(mi.tbl.out.ForeignKeys, &schema.ForeignKey{
			Columns:    []string{rel.colName},
			RefTable:   tgt.tbl.out.Name,
			RefColumns: []string{pk[0]},
		})
		mi.tbl.out.Relations = append(mi.tbl.out.Relations, &schema.Relation{
			Field:    rel.fieldName,
			Target:   tgt.tbl.out.Name,
			TargetGo: rel.target.Name(),
			Column:   rel.colName,
			Owner:    true,
			Nullable: rel.nullable,
		})
		mi.fkCols = append(mi.fkCols, rel.colName)
	}
	b.resolveArcs(mi)
}

// resolveArcs finishes the exclusive arcs: a foreign key per variant, the
// exactly-one CHECK, and a partial index per variant.
//
// Deferred to pass 3 for the same reason relations are — the referenced primary
// key does not exist until every table's columns are built.
func (b *builder) resolveArcs(mi *modelInfo) {
	for _, ac := range mi.arcCols {
		tgt := b.tableByName(ac.variant.Table)
		if tgt == nil {
			continue // already reported
		}
		pk := tgt.PrimaryKey
		if len(pk) != 1 {
			b.errs.add(fmt.Errorf(
				"%s: arc variant %s has a %d-column primary key; an arc needs a single-column key",
				mi.tbl.out.Name, ac.variant.Table, len(pk)))
			continue
		}
		if refCol := tgt.Column(pk[0]); refCol != nil {
			ac.col.sc.Type = refCol.Type
		}
		mi.tbl.out.ForeignKeys = append(mi.tbl.out.ForeignKeys, &schema.ForeignKey{
			Columns:    []string{ac.variant.Column},
			RefTable:   ac.variant.Table,
			RefColumns: []string{pk[0]},
			OnDelete:   schema.Cascade,
		})
		mi.fkCols = append(mi.fkCols, ac.variant.Column)
	}

	for _, arc := range mi.tbl.out.Arcs {
		mi.tbl.out.Checks = append(mi.tbl.out.Checks, &schema.Check{
			Name: "ck_" + mi.tbl.out.Name + "_" + snake(arc.Field),
			Expr: arcCheckExpr(arc),
		})
		// A partial index per variant. Without them, "the attachments of this
		// post" scans every attachment of every kind — and the whole reason to
		// keep separate columns is that each one can be indexed.
		for _, v := range arc.Variants {
			mi.tbl.out.Indexes = append(mi.tbl.out.Indexes, &schema.Index{
				Name:    "ix_" + mi.tbl.out.Name + "_" + v.Column,
				Columns: []schema.IndexColumn{{Name: v.Column}},
				Where:   quoteIdent(v.Column) + " IS NOT NULL",
			})
		}
	}
}

// arcCheckExpr is the exactly-one (or at-most-one) constraint.
//
// Summing booleans as ints rather than a chain of ORs and ANDs: it stays one
// readable expression as variants are added, and it says what it means —
// exactly one of these is present.
func arcCheckExpr(arc *schema.Arc) string {
	var b strings.Builder
	for i, v := range arc.Variants {
		if i > 0 {
			b.WriteString(" + ")
		}
		b.WriteString("(")
		b.WriteString(quoteIdent(v.Column))
		b.WriteString(" IS NOT NULL)::int")
	}
	if arc.Optional {
		b.WriteString(" <= 1")
	} else {
		b.WriteString(" = 1")
	}
	return b.String()
}

func (b *builder) tableByName(name string) *schema.Table {
	for _, mi := range b.ordered {
		if mi.tbl != nil && mi.tbl.out.Name == name {
			return mi.tbl.out
		}
	}
	return nil
}

func quoteIdent(s string) string { return `"` + s + `"` }

// callPlans runs a model's Plans method, if it has one.
//
// Optional by design: a model with no plans still gets the one-plan-per-
// relation tier, which is finite by construction. Declaring plans is how you
// ask for combinations, and only combinations can explode.
func (b *builder) callPlans(mi *modelInfo) {
	p, ok := mi.ptr.Interface().(Planner)
	if !ok {
		return
	}
	p.Plans(&Plans{t: mi.tbl, out: &mi.tbl.out.Plans, b: b})
}

// validateHasMany checks that every has-many has a matching key on the other
// side. A slice with no inverse is a silent no-op in most ORMs; here it fails
// the build.
func (b *builder) validateHasMany(mi *modelInfo) {
	for _, rel := range mi.pending {
		if !rel.toMany {
			continue
		}
		tgt := b.byType[rel.target]
		if tgt == nil || tgt.tbl == nil {
			continue // already reported
		}
		col := fkColumnTo(tgt.tbl.out, mi.tbl.out.Name)
		if col == "" {
			b.errs.add(fmt.Errorf(
				"%s.%s: has-many to %s, but %s has no field of type %s to carry the foreign key",
				mi.tbl.out.Name, rel.fieldName, rel.target.Name(),
				tgt.tbl.out.Name, mi.typ.Name()))
			continue
		}
		// Column is the CHILD's column, not this table's: a batch loader
		// filters children by it, and there is nothing on the parent to filter.
		mi.tbl.out.Relations = append(mi.tbl.out.Relations, &schema.Relation{
			Field:    rel.fieldName,
			Target:   tgt.tbl.out.Name,
			TargetGo: rel.target.Name(),
			ToMany:   true,
			Column:   col,
			Owner:    false,
		})
	}
}

// fkColumnTo is the column on t that references table, or "" if none does.
// A self-reference matches too — a hierarchy's children are found by the same
// mechanism as any other has-many.
func fkColumnTo(t *schema.Table, table string) string {
	for _, fk := range t.ForeignKeys {
		if fk.RefTable == table && len(fk.Columns) == 1 {
			return fk.Columns[0]
		}
	}
	return ""
}

func indexedFirst(t *schema.Table, col string) bool {
	for _, ix := range t.Indexes {
		if len(ix.Columns) > 0 && ix.Columns[0].Name == col {
			return true
		}
	}
	for _, u := range t.Uniques {
		if len(u.Columns) > 0 && u.Columns[0] == col {
			return true
		}
	}
	return len(t.PrimaryKey) > 0 && t.PrimaryKey[0] == col
}

// inferType maps a Go type onto a Postgres type.
func (b *builder) inferType(t reflect.Type) (schema.Type, bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A named type listing its own labels becomes a native enum.
	if t.Implements(enumerType) || reflect.PointerTo(t).Implements(enumerType) {
		v := reflect.New(t).Elem()
		if e, ok := v.Interface().(Enumer); ok {
			name := snake(t.Name())
			if _, seen := b.enums[name]; !seen {
				en := &schema.Enum{Name: name, Labels: e.EnumValues()}
				b.enums[name] = en
				b.outSch.Enums = append(b.outSch.Enums, en)
			}
			return schema.Type{Name: name, Enum: true}, true
		}
	}
	switch t {
	case timeType:
		return schema.Type{Name: schema.TypeTimestamptz}, true
	case uuidType:
		return schema.Type{Name: schema.TypeUUID}, true
	case bytesType:
		return schema.Type{Name: schema.TypeBytea}, true
	case intervalType:
		return schema.Type{Name: schema.TypeInterval}, true
	case prefixType:
		// netip.Prefix serves inet AND cidr: an inet is an address that may
		// carry a prefix (host bits allowed), a cidr is a network (host bits
		// forbidden, and the DATABASE polices that). One Go type; .Cidr() on
		// the column opts into the stricter database type.
		return schema.Type{Name: schema.TypeInet}, true
	case decimalType:
		// Precision and scale are unset here on purpose: numeric with no
		// bounds is legal and means "as much as you need". A model that wants
		// money says so with .Numeric(19, 4), and the generator refuses a
		// precision a Decimal cannot carry.
		return schema.Type{Name: schema.TypeNumeric}, true
	}
	switch t.Kind() {
	case reflect.Bool:
		return schema.Type{Name: schema.TypeBool}, true
	case reflect.Int16, reflect.Uint16:
		return schema.Type{Name: schema.TypeInt2}, true
	case reflect.Int32, reflect.Uint32:
		return schema.Type{Name: schema.TypeInt4}, true
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64:
		return schema.Type{Name: schema.TypeInt8}, true
	case reflect.Float32:
		return schema.Type{Name: schema.TypeFloat4}, true
	case reflect.Float64:
		return schema.Type{Name: schema.TypeFloat8}, true
	case reflect.String:
		return schema.Type{Name: schema.TypeText}, true
	case reflect.Array:
		if t.Len() == 16 && t.Elem().Kind() == reflect.Uint8 {
			return schema.Type{Name: schema.TypeUUID}, true
		}
	case reflect.Map:
		if t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.String {
			return schema.Type{Name: schema.TypeHstore}, true
		}
		return schema.Type{Name: schema.TypeJSONB}, true
	case reflect.Slice:
		el, ok := b.inferType(t.Elem())
		if !ok || el.Array {
			return schema.Type{}, false
		}
		el.Array = true
		return el, true
	case reflect.Struct:
		// Any other struct is a typed jsonb payload.
		return schema.Type{Name: schema.TypeJSONB}, true
	}
	return schema.Type{}, false
}

func baseName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t.Name()
}

func isNullable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Map:
		return true
	case reflect.Slice:
		return t == bytesType // []byte is nullable; T[] is not
	}
	return false
}

// ---- naming ----

func columnName(field string) string { return snake(field) }

func fkColumnName(field string) string { return snake(field) + "_id" }

// tableName pluralises naively. English is not derivable, so a Go type whose
// name is already plural (InferShapes) becomes infer_shapeses. Name models in
// the singular, or override with t.Name("...").
func tableName(model string) string {
	s := snake(model)
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"),
		strings.HasSuffix(s, "z"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !isVowel(s[len(s)-2]):
		return s[:len(s)-1] + "ies"
	default:
		return s + "s"
	}
}

func isVowel(c byte) bool { return strings.IndexByte("aeiou", c) >= 0 }

// snake converts CamelCase to snake_case, keeping acronyms intact:
// OrgID -> org_id, HTTPServer -> http_server, ID -> id.
func snake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	rs := []rune(s)
	for i, r := range rs {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			prevLower := rs[i-1] >= 'a' && rs[i-1] <= 'z' || rs[i-1] >= '0' && rs[i-1] <= '9'
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		if isUpper {
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// arcVariants reports whether t is a raorm.OneOfN and returns its variants.
//
// Recognised structurally rather than by name: every OneOfN is a zero-sized
// struct whose fields are all [0]T, so the variants are the element types. That
// means a new arity needs no change here, and a struct that merely happens to
// be called OneOf9 is not silently treated as one.
func arcVariants(t reflect.Type) (vs []reflect.Type, optional bool, ok bool) {
	optional = t.Kind() == reflect.Pointer
	if optional {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.NumField() == 0 || t.Size() != 0 {
		return nil, false, false
	}
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i).Type
		if ft.Kind() != reflect.Array || ft.Len() != 0 {
			return nil, false, false
		}
		vs = append(vs, ft.Elem())
	}
	return vs, optional, true
}

// buildArc emits a nullable foreign key per variant, a CHECK that the right
// number are set, and a partial index per variant.
func (b *builder) buildArc(mi *modelInfo, tbl *Table, f reflect.StructField,
	off uintptr, variants []reflect.Type, optional bool) {

	arc := &schema.Arc{Field: f.Name, Optional: optional}
	seen := map[string]bool{}
	for _, vt := range variants {
		tgt := b.byType[vt]
		if tgt == nil || tgt.tbl == nil {
			b.errs.add(fmt.Errorf(
				"%s.%s: variant %s is not registered — pass it to Build",
				mi.tbl.out.Name, f.Name, vt.Name()))
			return
		}
		if seen[vt.Name()] {
			b.errs.add(fmt.Errorf(
				"%s.%s: variant %s appears twice — the CHECK could never distinguish them",
				mi.tbl.out.Name, f.Name, vt.Name()))
			return
		}
		seen[vt.Name()] = true
		arc.Variants = append(arc.Variants, schema.ArcVariant{
			Table:  tgt.tbl.out.Name,
			GoName: vt.Name(),
			Column: snake(vt.Name()) + "_id",
		})
	}
	if len(arc.Variants) < 2 {
		b.errs.add(fmt.Errorf(
			"%s.%s: an arc over one variant is an ordinary relation — declare it as one",
			mi.tbl.out.Name, f.Name))
		return
	}

	// Columns first, so the CHECK and the indexes have something to name.
	for _, v := range arc.Variants {
		c := &col{
			sc: &schema.Column{
				Name: v.Column,
				Type: schema.Type{Name: schema.TypeUUID},
				// Every variant column is nullable: exactly one is set, so
				// the others must be able to be absent. The CHECK is what
				// makes "exactly one" true, not the column definitions.
			},
			field: f,
			isRel: true,
		}
		tbl.out.Columns = append(tbl.out.Columns, c.sc)
		mi.arcCols = append(mi.arcCols, arcCol{col: c, variant: v})
	}
	tbl.relOff[off] = f.Name
	tbl.out.Arcs = append(tbl.out.Arcs, arc)
}

// arcCol pairs a built column with the variant it came from, so pass 3 can add
// the foreign key once the target's primary key exists.
type arcCol struct {
	col     *col
	variant schema.ArcVariant
}
