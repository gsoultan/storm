package storm

import (
	"fmt"
	"reflect"

	"github.com/gsoultan/storm/schema"
)

// Named fetch plans.
//
// A plan says which relations are loaded together, and the generator emits a
// distinct type per plan whose fields are exactly those relations. Reading an
// unloaded relation is then a compile error rather than an empty slice.
//
// You name the plans. That is the whole answer to the projection-type
// explosion: generating a type per `With(...)` combination is 2ⁿ per entity,
// and the fix is not a cleverer generator but a shorter list — the one the
// developer actually uses.
//
// It also makes `plans.go` the single reviewable file listing every load
// pattern in a system, which is a thing no other Go ORM has, and which a
// linter can cost in round trips.
//
//	func (u *User) Plans(p *storm.Plans) {
//	    p.Named("Feed").With(&u.Posts).With(&u.Org)
//	}
//
// Relations are named by FIELD POINTER, like everything else in the
// declaration API, so the editor enforces them and a rename follows.
type Plans struct {
	t   *Table
	out *[]*schema.Plan
	b   *builder
}

// Nested names a relation on a table OTHER than the one declaring the plan —
// a post's comments, reached through a user's posts.
//
// It exists because a field pointer needs an instance to point into, and the
// declaring model has no Post to hand. Into supplies one: the builder already
// allocated a zero value of every registered model, so the closure is called
// with that and the offset resolves against the right table.
type Nested struct {
	typ     reflect.Type
	pick    func(ptr reflect.Value) any
	nested  []Nested
	invalid string
}

// Into names a relation on T, for use inside With.
//
//	p.Named("Feed").With(&u.Posts, storm.Into(func(p *Post) any { return &p.Comments }))
//
// The type parameter is what says which table the field belongs to, so a
// pointer into the wrong model is a compile error rather than a mis-resolved
// offset.
func Into[T any](pick func(*T) any, nested ...Nested) Nested {
	var zero *T
	return Nested{
		typ:    reflect.TypeOf(zero).Elem(),
		pick:   func(ptr reflect.Value) any { return pick(ptr.Interface().(*T)) },
		nested: nested,
	}
}

// Planner is implemented by models that declare fetch plans. Optional: a model
// with no Plans method gets the one-plan-per-relation tier, which is finite by
// construction and needs no declaration.
type Planner interface {
	Plans(*Plans)
}

// Named starts a plan. The generated type is the model name plus this name, so
// "Feed" on User becomes UserFeed.
func (p *Plans) Named(name string) *PlanBuilder {
	if !isExportedIdent(name) {
		p.t.errs.add(fmt.Errorf(
			"%s: plan name %q must be a valid exported Go identifier — it becomes a type name",
			p.t.out.Name, name))
		return &PlanBuilder{p: p, plan: &schema.Plan{Name: "Invalid"}}
	}
	for _, ex := range *p.out {
		if ex.Name == name {
			p.t.errs.add(fmt.Errorf("%s: plan %q is declared twice", p.t.out.Name, name))
			return &PlanBuilder{p: p, plan: &schema.Plan{Name: name}}
		}
	}
	pl := &schema.Plan{Name: name}
	*p.out = append(*p.out, pl)
	return &PlanBuilder{p: p, plan: pl}
}

// PlanBuilder collects the relations one plan loads.
type PlanBuilder struct {
	p    *Plans
	plan *schema.Plan
}

// With adds a relation to the plan, addressed by field pointer. Anything passed
// after it is loaded THROUGH it and costs one more round trip each.
func (b *PlanBuilder) With(relPtr any, nested ...Nested) *PlanBuilder {
	name, err := b.p.t.resolveRel(relPtr)
	if err != nil {
		b.p.t.errs.add(fmt.Errorf("%s: plan %q: %w", b.p.t.out.Name, b.plan.Name, err))
		return b
	}
	for _, ex := range b.plan.Fields {
		if ex.Field == name {
			b.p.t.errs.add(fmt.Errorf(
				"%s: plan %q loads %s twice", b.p.t.out.Name, b.plan.Name, name))
			return b
		}
	}
	f := schema.PlanField{Field: name}
	f.Nested = b.p.resolveNested(b.plan.Name, name, nested)
	b.plan.Fields = append(b.plan.Fields, f)
	return b
}

// resolveNested turns Into(...) values into field names on their own tables.
func (p *Plans) resolveNested(plan, through string, nested []Nested) []schema.PlanField {
	var out []schema.PlanField
	for _, n := range nested {
		mi := p.b.byType[n.typ]
		if mi == nil || mi.tbl == nil {
			p.t.errs.add(fmt.Errorf(
				"%s: plan %q: %s is not registered — pass it to Build",
				p.t.out.Name, plan, n.typ.Name()))
			continue
		}
		name, err := mi.tbl.resolveRel(n.pick(mi.ptr))
		if err != nil {
			p.t.errs.add(fmt.Errorf(
				"%s: plan %q: through %s into %s: %w",
				p.t.out.Name, plan, through, n.typ.Name(), err))
			continue
		}
		f := schema.PlanField{Field: name}
		f.Nested = p.resolveNested(plan, name, n.nested)
		out = append(out, f)
	}
	return out
}

// resolveRel turns a relation field pointer into its Go field name.
func (t *Table) resolveRel(fieldPtr any) (string, error) {
	v := reflect.ValueOf(fieldPtr)
	if v.Kind() != reflect.Pointer {
		return "", fmt.Errorf("want a field pointer, got %T", fieldPtr)
	}
	if v.IsNil() {
		return "", fmt.Errorf("nil field pointer")
	}
	base := uintptr(t.base)
	got := v.Pointer()
	if got < base || got >= base+t.typ.Size() {
		return "", fmt.Errorf(
			"field pointer does not point into the model — Plans must use a POINTER receiver "+
				"(func (m *%s) Plans(p *storm.Plans)); a value receiver copies the struct first",
			t.typ.Name())
	}
	name, ok := t.relOff[got-base]
	if !ok {
		return "", fmt.Errorf(
			"the field at offset %d is not a relation — a plan loads relations, and a "+
				"scalar column is already on the row", got-base)
	}
	return name, nil
}

// isExportedIdent reports whether s can be pasted into a generated type name.
func isExportedIdent(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}
