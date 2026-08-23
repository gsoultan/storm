package raorm

import (
	"fmt"
	"reflect"

	"github.com/gsoultan/raorm/schema"
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
//	func (u *User) Plans(p *raorm.Plans) {
//	    p.Named("Feed").With(&u.Posts).With(&u.Org)
//	}
//
// Relations are named by FIELD POINTER, like everything else in the
// declaration API, so the editor enforces them and a rename follows.
type Plans struct {
	t   *Table
	out *[]*schema.Plan
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

// With adds a relation to the plan, addressed by field pointer.
func (b *PlanBuilder) With(relPtr any) *PlanBuilder {
	name, err := b.p.t.resolveRel(relPtr)
	if err != nil {
		b.p.t.errs.add(fmt.Errorf("%s: plan %q: %w", b.p.t.out.Name, b.plan.Name, err))
		return b
	}
	for _, ex := range b.plan.Fields {
		if ex == name {
			b.p.t.errs.add(fmt.Errorf(
				"%s: plan %q loads %s twice", b.p.t.out.Name, b.plan.Name, name))
			return b
		}
	}
	b.plan.Fields = append(b.plan.Fields, name)
	return b
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
				"(func (m *%s) Plans(p *raorm.Plans)); a value receiver copies the struct first",
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
