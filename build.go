package raorm

import (
	"fmt"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/gsoultan/raorm/schema"
)

var (
	timeType   = reflect.TypeOf(time.Time{})
	uuidType   = reflect.TypeOf(UUID{})
	bytesType  = reflect.TypeOf([]byte(nil))
	enumerType = reflect.TypeOf((*Enumer)(nil)).Elem()
	schemerTyp = reflect.TypeOf((*Schemer)(nil)).Elem()
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
	st := &schema.Table{Name: tableName(mi.typ.Name())}
	tbl := &Table{
		base: unsafe.Pointer(mi.ptr.Pointer()),
		typ:  mi.typ,
		out:  st,
		off:  map[uintptr]*col{},
		errs: b.errs,
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

		// Relation?
		if rel := b.relationFor(f); rel != nil {
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
		mi.fkCols = append(mi.fkCols, rel.colName)
	}
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
		if !hasFKTo(tgt.tbl.out, mi.tbl.out.Name) {
			b.errs.add(fmt.Errorf(
				"%s.%s: has-many to %s, but %s has no field of type %s to carry the foreign key",
				mi.tbl.out.Name, rel.fieldName, rel.target.Name(),
				tgt.tbl.out.Name, mi.typ.Name()))
		}
	}
}

func hasFKTo(t *schema.Table, table string) bool {
	for _, fk := range t.ForeignKeys {
		if fk.RefTable == table {
			return true
		}
	}
	return false
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
