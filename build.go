package storm

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
)

var (
	timeType      = reflect.TypeOf(time.Time{})
	intervalType  = reflect.TypeOf(runtime.Interval{})
	timeOfDayType = reflect.TypeOf(runtime.TimeOfDay(0))
	prefixType    = reflect.TypeOf(netip.Prefix{})
	uuidType      = reflect.TypeOf(UUID{})
	bytesType     = reflect.TypeOf([]byte(nil))
	decimalType   = reflect.TypeOf(Decimal{})
	tsvectorType  = reflect.TypeOf(TSVector{})
	tstzRangeType = reflect.TypeOf(TstzRange{})
	enumerType    = reflect.TypeOf((*Enumer)(nil)).Elem()
	schemerTyp    = reflect.TypeOf((*Schemer)(nil)).Elem()
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

	// Pass 4: user declarations last, so OnDelete has a foreign key to attach
	// to and an explicit setting always wins over an inferred one.
	for _, mi := range b.ordered {
		b.callSchemas(mi)
	}
	// Pass 4a: the AnyRef acknowledgement, which IS a user declaration and so
	// cannot be checked before they have run.
	for _, mi := range b.ordered {
		b.checkAnyRefsAcknowledged(mi)
	}
	// Pass 4b: t.Through, which is also a user declaration and needs the join
	// model's own foreign keys — built in pass 3, named here.
	for _, mi := range b.ordered {
		b.resolveThrough(mi)
	}
	// Pass 4c: validate has-many, and recognise implicit many-to-many.
	//
	// AFTER the user declarations, not before. A has-many with no key on the
	// far side is an error unless something claims it, and t.Through is one of
	// the things that can — running this first reported "no field to carry the
	// foreign key" for a relation the model had already explained.
	for _, mi := range b.ordered {
		b.validateHasMany(mi)
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
		b.callAggregates(mi)
	}
	// Joins last: they reference other tables' columns and other tables'
	// declared aggregations, so every table has to be complete first.
	for _, mi := range b.ordered {
		b.callJoins(mi)
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
	// selfLink is the field of the one self-referential many-to-many this
	// model is allowed. A second is ambiguous rather than unsupported.
	selfLink string
	fkCols   []string
	arcCols  []arcCol
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
				"(func (m *%s) Schema(t *storm.Table)), or field pointers cannot be resolved",
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
		// An unexported embedded field cannot be read back through reflect —
		// Interface() panics on one — so this has to be decided from the TYPE.
		// Skipping silently would be worse than the panic it replaces: the
		// mixin's Schema would never run, and the table would come out missing
		// declarations nobody could see were missing.
		if f.PkgPath != "" {
			if reflect.PointerTo(f.Type).Implements(schemerTyp) {
				b.errs.add(fmt.Errorf(
					"%s embeds %s, which is unexported and has a Schema method — "+
						"reflect cannot reach an unexported embedded field, so that "+
						"Schema would never run and the table would be missing whatever "+
						"it declares\n"+
						"       export the mixin; being embedded is what makes it a mixin, "+
						"not being unexported",
					t.Name(), f.Type.Name()))
			}
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

		// Discriminator polymorphism? A (type, id) pair and no foreign key.
		if f.Type == anyRefType {
			b.buildAnyRef(mi, tbl, f, off)
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
			b.errs.add(fmt.Errorf("%s.%s: %s embeds storm.Model but is not registered — pass it to Build",
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

// looksLikeModel reports whether a struct embeds storm.Model. Such a field was
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

// checkAnyRefsAcknowledged is the whole reason AnyRef is a distinct type rather
// than two columns someone declares by hand.
//
// Two columns are ordinary and pass without comment. A declaration that names
// the shape can be refused until somebody writes down why the integrity is
// being given up — and the refusal happens at Build, so it is a failed
// generation rather than a code review nobody ran.
func (b *builder) checkAnyRefsAcknowledged(mi *modelInfo) {
	for _, ar := range mi.tbl.out.AnyRefs {
		if ar.Reason != "" {
			continue
		}
		b.errs.add(fmt.Errorf(
			"%s.%s: storm.AnyRef gives up referential integrity — orphan rows are "+
				"possible and no database constraint will prevent them\n"+
				"       prefer storm.OneOf[...] (up to 8 variants, keys enforced), a "+
				"supertype table (unbounded, still enforced), or acknowledge it:\n"+
				"       t.Col(&x.%s).AcknowledgeNoFK(\"<why>\")",
			mi.tbl.out.Name, ar.Field, ar.Field))
	}
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
// callAggregates collects declared aggregations. Same pass as plans: both are
// declarations over a table whose columns are already resolved.
func (b *builder) callAggregates(mi *modelInfo) {
	a, ok := mi.ptr.Interface().(Aggregator)
	if !ok {
		return
	}
	a.Aggregates(&Aggregates{t: mi.tbl, out: &mi.tbl.out.Aggregates})

	// Validated after the whole declaration is read, because "is this column
	// grouped?" cannot be answered until every By has been seen — a chain
	// declares its outputs before its grouping just as often as after.
	for _, agg := range mi.tbl.out.Aggregates {
		if err := validateAggregate(mi.tbl.out, agg); err != nil {
			b.errs.add(err)
		}
	}
}

// infoFor finds the registered model a value belongs to.
func (b *builder) infoFor(model any) *modelInfo {
	t := reflect.TypeOf(model)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return nil
	}
	return b.byType[t]
}

// callJoins collects declared joins.
//
// A separate pass from plans and aggregates because a join names OTHER tables,
// and those tables' columns are only resolved once every model has been walked.
func (b *builder) callJoins(mi *modelInfo) {
	j, ok := mi.ptr.Interface().(Joiner)
	if !ok {
		return
	}
	j.Joins(&Joins{t: mi.tbl, b: b})
}

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
			// No key on the far side. Before this is an error, ask whether the
			// far side is a slice back to here — a slice on both sides is a
			// MANY-TO-MANY, and the key neither of them carries lives on a
			// join table.
			if b.linkFor(mi, tgt, rel) {
				continue
			}
			b.errs.add(fmt.Errorf(
				"%s.%s: has-many to %s, but %s has no field of type %s to carry the foreign key\n"+
					"       add one, or declare the inverse as a slice too — a slice on both "+
					"sides is a many-to-many and storm generates the join table",
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

// linkFor recognises a many-to-many and wires both sides to a join table,
// generating the table itself the first time it is asked.
//
// Reports whether rel is one half of a many-to-many. It is called only when
// the far side carries no key, which is the whole signal: a has-many whose
// inverse is also a has-many is the shape no single foreign key can express.
//
// Runs once per PAIR, not once per side. The second side finds the table
// already built and joins it, so the two agree on names by construction rather
// than by computing the same thing twice.
func (b *builder) linkFor(mi *modelInfo, tgt *modelInfo, rel *relation) bool {
	// The far side must name this type back, as a slice.
	var back *relation
	for _, r := range tgt.pending {
		if r.toMany && r.target == mi.typ {
			back = r
			break
		}
	}
	if back == nil {
		return false
	}

	here, there := mi.tbl.out, tgt.tbl.out
	link := linkTableName(here.Name, there.Name)
	hereCol := singular(here.Name) + "_id"
	thereCol := singular(there.Name) + "_id"

	// A self-referential many-to-many — a graph of posts related to posts —
	// would name both columns the same thing if they came from the table. They
	// come from the FIELD instead: Post.Related gives post_related(post_id,
	// related_id), where the second column is named for the role it plays.
	//
	// The edge is DIRECTED and stored once. storm does not invent the reverse:
	// inserting A→B does not make B→A, because "related to" and "follows" are
	// both spelled this way and only one of them is symmetric. A model that
	// wants both directions writes both rows.
	if here.Name == there.Name {
		if !b.claimSelfLink(mi, rel) {
			return true
		}
		link = singular(here.Name) + "_" + snake(rel.fieldName)
		thereCol = snake(rel.fieldName) + "_id"
	}

	if b.outSch.Table(link) == nil {
		b.outSch.Tables = append(b.outSch.Tables, b.buildLinkTable(link, here, there, hereCol, thereCol))
	}

	add := func(owner *schema.Table, r *relation, target *schema.Table, own, far string) {
		owner.Relations = append(owner.Relations, &schema.Relation{
			Field:            r.fieldName,
			Target:           target.Name,
			TargetGo:         goNameOf(target),
			ToMany:           true,
			Owner:            false,
			Link:             link,
			LinkColumn:       own,
			LinkTargetColumn: far,
		})
	}
	add(here, rel, there, hereCol, thereCol)
	if back == rel {
		// Self-referential: the scan for the far side found THIS relation,
		// because the far model is this model. Wiring it again would put two
		// `Related` fields on one table pointing opposite ways down the same
		// join — a duplicate the generator then emits twice.
		return true
	}
	add(there, back, here, thereCol, hereCol)
	// The far side is wired now; leaving it pending would report it as an
	// error when its own table is validated.
	back.toMany = false
	return true
}

// resolveThrough wires a t.Through declaration: a many-to-many over a join
// model the adopter wrote, whose columns storm reads rather than invents.
//
// The join model must carry exactly one foreign key to each end. That is the
// whole disambiguation: with one key to each, "which column points at me" has
// one answer, and storm needs no further declaration. With two to the same
// table it does not, and says so.
func (b *builder) resolveThrough(mi *modelInfo) {
	for _, d := range mi.tbl.through {
		var rel *relation
		for _, r := range mi.pending {
			if r.fieldName == d.field {
				rel = r
				break
			}
		}
		if rel == nil {
			b.errs.add(fmt.Errorf("%s.%s: Through names a field that is not a relation",
				mi.tbl.out.Name, d.field))
			continue
		}
		jm := b.byType[d.join]
		if jm == nil || jm.tbl == nil {
			b.errs.add(fmt.Errorf(
				"%s.%s: the join model %s is not registered — pass it to Build",
				mi.tbl.out.Name, d.field, d.join.Name()))
			continue
		}
		tgt := b.byType[rel.target]
		if tgt == nil || tgt.tbl == nil {
			continue // already reported
		}

		join, here, there := jm.tbl.out, mi.tbl.out, tgt.tbl.out
		own, err := soleFKTo(join, here.Name)
		if err != nil {
			b.errs.add(fmt.Errorf("%s.%s: %s %w", here.Name, d.field, join.Name, err))
			continue
		}
		far, err := soleFKTo(join, there.Name)
		if err != nil {
			b.errs.add(fmt.Errorf("%s.%s: %s %w", here.Name, d.field, join.Name, err))
			continue
		}

		// Payload is what makes this shape worth writing by hand: the join
		// carries columns beyond its two keys, and the plan row exposes them.
		payload := false
		for _, c := range join.Columns {
			if c.Name != own && c.Name != far {
				payload = true
				break
			}
		}

		here.Relations = append(here.Relations, &schema.Relation{
			Field:            rel.fieldName,
			Target:           there.Name,
			TargetGo:         there.GoName,
			ToMany:           true,
			Owner:            false,
			Link:             join.Name,
			LinkColumn:       own,
			LinkTargetColumn: far,
			LinkPayload:      payload,
		})
		// Wired here, so validateHasMany must not report it as keyless.
		rel.toMany = false
	}
}

// soleFKTo is the join model's one foreign key to table.
//
// Exactly one, deliberately. Two keys to the same table is a self-referential
// join whose direction only the adopter knows, and guessing would silently
// load the wrong end.
func soleFKTo(join *schema.Table, table string) (string, error) {
	var found []string
	for _, fk := range join.ForeignKeys {
		if fk.RefTable == table && len(fk.Columns) == 1 {
			found = append(found, fk.Columns[0])
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("has no foreign key to %s", table)
	default:
		return "", fmt.Errorf(
			"has %d foreign keys to %s (%s) and storm cannot tell which end is which",
			len(found), table, strings.Join(found, ", "))
	}
}

// claimSelfLink allows exactly one self-referential many-to-many per model.
//
// Two of them — the Following/Followers pair everybody reaches for — are one
// relationship seen from both ends, and storm has no way to know that: it would
// generate two unrelated tables, so following somebody would not make you their
// follower. That is a wrong answer, not a missing feature, so it is refused
// with the shape that does work.
func (b *builder) claimSelfLink(mi *modelInfo, rel *relation) bool {
	if prev := mi.selfLink; prev != "" {
		b.errs.add(fmt.Errorf(
			"%s: %s and %s are both self-referential many-to-many fields, and storm "+
				"cannot tell whether they are one relationship or two\n"+
				"       two generated tables would mean following someone does not make "+
				"you their follower\n"+
				"       declare the join model yourself with two named foreign keys, and "+
				"traverse it as two hops",
			mi.tbl.out.Name, prev, rel.fieldName))
		return false
	}
	mi.selfLink = rel.fieldName
	return true
}

// buildLinkTable is the join table of an implicit many-to-many: two keys, a
// composite primary key over both, and a cascade on each.
//
// CASCADE rather than RESTRICT because the row means "these two are related",
// and deleting either end makes the statement meaningless rather than
// dangling. It never deletes anything the adopter can see: the far ROW
// survives, only the association goes.
func (b *builder) buildLinkTable(name string, a, c *schema.Table, aCol, cCol string) *schema.Table {
	t := &schema.Table{
		Name: name,
		// A singular Go name, so the generated package and Row type read as
		// one link — postTag, not postTags. Set here rather than left empty
		// because PackageName falls back to the TABLE name, and the plan that
		// references this package derives its own name from GoName: the two
		// must agree, and the only way to be sure is for one value to feed
		// both.
		GoName:    schema.GoName(singular(name)),
		Generated: true,
		Columns: []*schema.Column{
			{Name: aCol, Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: cCol, Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		},
		PrimaryKey: []string{aCol, cCol},
		ForeignKeys: []*schema.ForeignKey{
			{Name: "fk_" + name + "_" + aCol, Columns: []string{aCol},
				RefTable: a.Name, RefColumns: a.PrimaryKey, OnDelete: schema.Cascade},
			{Name: "fk_" + name + "_" + cCol, Columns: []string{cCol},
				RefTable: c.Name, RefColumns: c.PrimaryKey, OnDelete: schema.Cascade},
		},
		// The composite primary key indexes (aCol, cCol), which serves a
		// lookup from a but not one from c. The second index is what keeps the
		// reverse direction from a sequential scan.
		Indexes: []*schema.Index{{Columns: []schema.IndexColumn{{Name: cCol}, {Name: aCol}}}},
	}
	return t
}

// linkTableName is the join table for two tables, and must not depend on which
// side the walker reached first. Sorted, so `posts` and `tags` give
// `post_tags` from either direction.
func linkTableName(a, c string) string {
	if a > c {
		a, c = c, a
	}
	return singular(a) + "_" + c
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
	case tstzRangeType:
		return schema.Type{Name: schema.TypeTstzRange}, true
	case tsvectorType:
		// Matched before the Kind() switch: TSVector is an empty struct, and
		// a struct with no fields would otherwise fall through to
		// "unsupported" rather than to the search column it is.
		return schema.Type{Name: schema.TypeTSVector}, true
	case timeType:
		return schema.Type{Name: schema.TypeTimestamptz}, true
	case uuidType:
		return schema.Type{Name: schema.TypeUUID}, true
	case bytesType:
		return schema.Type{Name: schema.TypeBytea}, true
	case intervalType:
		return schema.Type{Name: schema.TypeInterval}, true
	case timeOfDayType:
		// Matched here, before the Kind() switch below, because TimeOfDay's
		// underlying kind is int64 and would otherwise land as int8.
		return schema.Type{Name: schema.TypeTime}, true
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

// singular and goNameOf are the join-table naming rules, both delegating so
// that schema, codegen and the builder cannot disagree about a column name.
func singular(s string) string { return schema.Singular(s) }

func goNameOf(t *schema.Table) string { return t.GoName }

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

// arcVariants reports whether t is a storm.OneOfN and returns its variants.
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

var anyRefType = reflect.TypeOf(AnyRef{})

// buildAnyRef emits the (type, id) column pair and records the declaration.
//
// No foreign key and no CHECK, because there is nothing to point at: the whole
// shape is that the target table is not known until the row is read. What it
// does get is a composite index, since every read of a discriminator pair
// filters on both columns and neither alone is selective.
func (b *builder) buildAnyRef(mi *modelInfo, tbl *Table, f reflect.StructField, off uintptr) {
	base := snake(f.Name)
	ar := &schema.AnyRefField{
		Field:      f.Name,
		TypeColumn: base + "_type",
		IDColumn:   base + "_id",
	}

	// The TYPE column carries the back-pointer, so t.Col(&a.Subject) resolves
	// to something AcknowledgeNoFK can reach.
	tc := &col{
		sc:     &schema.Column{Name: ar.TypeColumn, Type: schema.Type{Name: schema.TypeText}, NotNull: true},
		field:  f,
		anyRef: ar,
	}
	ic := &col{
		sc:    &schema.Column{Name: ar.IDColumn, Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		field: f,
	}
	tbl.out.Columns = append(tbl.out.Columns, tc.sc, ic.sc)
	tbl.off[off] = tc

	// Both columns, in this order: a lookup names the type first and the id
	// second, and a btree on (type, id) serves that prefix.
	tbl.out.Indexes = append(tbl.out.Indexes, &schema.Index{
		Columns: []schema.IndexColumn{{Name: ar.TypeColumn}, {Name: ar.IDColumn}},
	})
	tbl.out.AnyRefs = append(tbl.out.AnyRefs, ar)
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
