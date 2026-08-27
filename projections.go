package storm

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// Named projections: read less than the whole row, by name.
//
//	func (u *User) Projections(p *storm.Projections) {
//	    p.Named("Contact", &u.Email, &u.Name)
//	}
//
//	rows, err := user.New().Where(...).AllContact(ctx, ex)   // []user.ContactRow
//
// The full-row read is the safe default and the expensive one: every column
// travels, TOAST'd values are fetched whether or not anyone looks, and an
// index-only scan is impossible by construction. A named projection is the
// declared, reviewable subset — same predicates, same ordering, same keyset
// machinery, narrower tuple, its own generated row type and scanner.
//
// Named rather than a Select(cols...) builder for R3's reason: a type per
// combination is 2ⁿ per entity, and the fix is a shorter list — the one the
// code actually uses.
type Projections struct {
	t   *Table
	out *[]*schema.Projection
}

// Projector is implemented by models that declare projections. Optional.
type Projector interface {
	Projections(*Projections)
}

// Named declares one projection over the given column fields.
func (p *Projections) Named(name string, fieldPtrs ...any) {
	if !isExportedIdent(name) {
		p.t.errs.add(fmt.Errorf(
			"%s: projection name %q must be a valid exported Go identifier — it becomes a type name",
			p.t.out.Name, name))
		return
	}
	if name == "Into" {
		// AllInto already exists on every Query; All+Name must stay unambiguous.
		p.t.errs.add(fmt.Errorf("%s: projection name %q is reserved", p.t.out.Name, name))
		return
	}
	for _, ex := range *p.out {
		if ex.Name == name {
			p.t.errs.add(fmt.Errorf("%s: projection %q is declared twice", p.t.out.Name, name))
			return
		}
	}
	if len(fieldPtrs) == 0 {
		p.t.errs.add(fmt.Errorf(
			"%s: projection %q selects no columns — a projection exists to read less, not nothing",
			p.t.out.Name, name))
		return
	}

	pr := &schema.Projection{Name: name}
	seen := map[string]bool{}
	for _, fp := range fieldPtrs {
		c, err := p.t.resolve(fp)
		if err != nil {
			p.t.errs.add(fmt.Errorf("%s: projection %q: %w", p.t.out.Name, name, err))
			return
		}
		if seen[c.sc.Name] {
			p.t.errs.add(fmt.Errorf(
				"%s: projection %q selects %s twice", p.t.out.Name, name, c.sc.Name))
			return
		}
		seen[c.sc.Name] = true
		pr.Columns = append(pr.Columns, c.sc.Name)
	}
	*p.out = append(*p.out, pr)
}
