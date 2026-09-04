package storm

import (
	"fmt"
	"reflect"

	"github.com/gsoultan/storm/schema"
)

// A declared UNION: several tables projected into one row shape.
//
//	var Feed = storm.Union("Feed", func(u *storm.UnionSpec) {
//	    var c Comment
//	    b := u.From(&c)
//	    b.Take(&c.CreatedAt, "OccurredAt")
//	    b.Take(&c.Body, "Text")
//	    b.Const("Kind", "comment")
//
//	    var r Release
//	    b2 := u.From(&r)
//	    b2.Take(&r.PublishedAt, "OccurredAt")
//	    b2.Take(&r.Notes, "Text")
//	    b2.Const("Kind", "release")
//
//	    u.OrderDesc("OccurredAt")
//	})
//
// # Why this is a package-level var and not a method
//
// Every other cross-table read hangs off a DRIVING table: a declared join is a
// method on the model that declares it, and its row type lives in that model's
// package. A union has no such centre. In a feed of comments, follows and
// releases none of the three is the one the others attach to, and declaring it
// on whichever sorted first would put the row type in a package with no more
// claim to it than the other two. So a union is registered against the schema
// (ADR-0008).
//
// # Why a closure
//
// The branches need LOCAL model instances to take field pointers into, exactly
// as a Joins method does — and a package-level var cannot have locals. The
// closure runs during Build, when the tables exist and the pointers can be
// resolved; nothing is evaluated at package init but the name.
func Union(name string, fn func(*UnionSpec)) *UnionDecl {
	return &UnionDecl{name: name, fn: fn}
}

// UnionDecl is a declared union, before it has been resolved against a schema.
// Pass it to Build alongside the models.
type UnionDecl struct {
	name string
	fn   func(*UnionSpec)
}

// Name reports the declared name; the generate command uses it.
func (d *UnionDecl) Name() string { return d.name }

// UnionSpec is a union under construction.
type UnionSpec struct {
	b    *builder
	u    *schema.Union
	dead bool
}

// UnionBranchSpec is one branch of a union under construction.
type UnionBranchSpec struct {
	s    *UnionSpec
	i    int // index into s.u.Branches
	base uintptr
	size uintptr
	tbl  *Table
	dead bool
}

// From starts a branch reading model's table.
func (s *UnionSpec) From(model any) *UnionBranchSpec {
	if s.dead {
		return &UnionBranchSpec{s: s, dead: true}
	}
	mi := s.b.infoFor(model)
	if mi == nil {
		s.fail("reads %T, which is not a registered model", model)
		return &UnionBranchSpec{s: s, dead: true}
	}
	s.u.Branches = append(s.u.Branches, schema.UnionBranch{Table: mi.tbl.out.Name})
	return &UnionBranchSpec{
		s:    s,
		i:    len(s.u.Branches) - 1,
		base: reflect.ValueOf(model).Pointer(),
		size: reflect.TypeOf(model).Elem().Size(),
		tbl:  mi.tbl,
	}
}

// Take projects a column into the output column named as.
//
// Every branch must project the same names in the same order. That is what
// union-compatible means, and storm checks it rather than trusting the
// declaration: two branches whose third column is `Text` in one and `Kind` in
// the other produce a row type where half the values are in the wrong field,
// and PostgreSQL will not object as long as the types line up.
func (b *UnionBranchSpec) Take(fieldPtr any, as string) *UnionBranchSpec {
	if b.dead || b.s.dead {
		return b
	}
	if !isExportedIdent(as) {
		b.s.fail("output %q must be a valid exported Go identifier", as)
		return b
	}
	c, err := b.column(fieldPtr)
	if err != nil {
		b.s.fail("taking %q: %w", as, err)
		return b
	}
	return b.push(as, schema.Expr{
		Kind: schema.ExprCol, Col: c.Name, Type: c.Type, Nullable: !c.NotNull,
	}, c.Type, !c.NotNull)
}

// Const projects a literal, which is how a merged feed carries the tag saying
// which branch a row came from. Without it the rows are indistinguishable once
// they are merged, and the caller is left inferring the source from which
// fields happen to be set.
func (b *UnionBranchSpec) Const(as string, v any) *UnionBranchSpec {
	if b.dead || b.s.dead {
		return b
	}
	if !isExportedIdent(as) {
		b.s.fail("output %q must be a valid exported Go identifier", as)
		return b
	}
	t := lit(v)
	if t.err != nil {
		b.s.fail("constant %q: %w", as, t.err)
		return b
	}
	ty := schema.Type{Name: t.lit.Kind}
	return b.push(as, schema.Expr{Kind: schema.ExprLit, Lit: t.lit, Type: ty}, ty, false)
}

// Where filters this branch. Declared, so it is fixed in the statement text —
// there is no call-site predicate on a union, because a predicate over several
// branches would have to say which one it filtered.
func (b *UnionBranchSpec) Where(c Cond) *UnionBranchSpec {
	if b.dead || b.s.dead {
		return b
	}
	br := &b.s.u.Branches[b.i]
	if br.Where != nil {
		b.s.fail("branch %d declares Where twice — combine them with storm.And", b.i+1)
		return b
	}
	sc, err := b.resolveCond(c)
	if err != nil {
		b.s.fail("branch %d filter: %w", b.i+1, err)
		return b
	}
	br.Where = &sc
	return b
}

// OrderAsc and OrderDesc order the MERGED rows. They name output columns:
// after the branches are unioned the source tables' own names are gone, and an
// output alias is the only thing left in scope.
func (s *UnionSpec) OrderAsc(col string) *UnionSpec  { return s.order(col, false) }
func (s *UnionSpec) OrderDesc(col string) *UnionSpec { return s.order(col, true) }

func (s *UnionSpec) order(col string, desc bool) *UnionSpec {
	if s.dead {
		return s
	}
	s.u.OrderBy = append(s.u.OrderBy, schema.UnionOrder{Col: col, Desc: desc})
	return s
}

// Distinct selects UNION over UNION ALL.
//
// ALL is the default, inverting SQL's. De-duplicating means sorting or hashing
// the whole result before a single row comes back, and a feed never wants it;
// a caller who does says so here.
func (s *UnionSpec) Distinct() *UnionSpec {
	if !s.dead {
		s.u.Distinct = true
	}
	return s
}

func (b *UnionBranchSpec) push(as string, e schema.Expr, ty schema.Type, nullable bool) *UnionBranchSpec {
	br := &b.s.u.Branches[b.i]
	br.Exprs = append(br.Exprs, e)

	pos := len(br.Exprs) - 1
	u := b.s.u
	if b.i == 0 {
		// The first branch DEFINES the shape.
		u.Cols = append(u.Cols, schema.UnionCol{As: as, Type: ty, Nullable: nullable})
		return b
	}
	if pos >= len(u.Cols) {
		b.s.fail("branch %d projects %d column(s) but the first projects %d",
			b.i+1, pos+1, len(u.Cols))
		return b
	}
	want := &u.Cols[pos]
	if want.As != as {
		b.s.fail("branch %d has %q at position %d where the first has %q — "+
			"the branches of a union must project the same names in the same order, "+
			"or values land in the wrong fields",
			b.i+1, as, pos+1, want.As)
		return b
	}
	unified, err := schema.UnifyUnion(want.Type, ty)
	if err != nil {
		b.s.fail("column %q is %s in branch %d and %s in the first — %w",
			as, ty.Name, b.i+1, want.Type.Name, err)
		return b
	}
	// text beside varchar(300) is text, int4 beside int8 is int8: PostgreSQL
	// widens a union column and sends the wider type, so the row must carry it.
	want.Type = unified
	// Nullable if ANY branch can produce NULL here: typing it otherwise would
	// decode a NULL from one branch as a zero value.
	want.Nullable = want.Nullable || nullable
	return b
}

// resolveCond turns a declaration Cond into schema form, against THIS branch's
// table. A branch filter can only name that branch's own columns: it is
// rendered inside that branch's SELECT, before anything is merged.
func (b *UnionBranchSpec) resolveCond(c Cond) (schema.Cond, error) {
	switch c.kind {
	case schema.CondCmp:
		l, err := b.resolveTerm(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		r, err := b.resolveTerm(c.right)
		if err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: schema.CondCmp, Op: c.op, Left: l, Right: r}, nil

	case schema.CondIsNull, schema.CondIsNotNull:
		l, err := b.resolveTerm(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: c.kind, Left: l}, nil

	default:
		if len(c.args) == 0 {
			return schema.Cond{}, fmt.Errorf("empty condition")
		}
		args := make([]schema.Cond, 0, len(c.args))
		for _, a := range c.args {
			sa, err := b.resolveCond(a)
			if err != nil {
				return schema.Cond{}, err
			}
			args = append(args, sa)
		}
		return schema.Cond{Kind: c.kind, Args: args}, nil
	}
}

// resolveTerm handles the two things a branch filter can contain: one of this
// branch's columns, and a literal fixed at declaration time.
func (b *UnionBranchSpec) resolveTerm(t Term) (schema.Expr, error) {
	if t.err != nil {
		return schema.Expr{}, t.err
	}
	switch t.kind {
	case schema.ExprLit:
		return schema.Expr{Kind: schema.ExprLit, Lit: t.lit,
			Type: schema.Type{Name: t.lit.Kind}}, nil
	case schema.ExprCol:
		c, err := b.column(t.fp)
		if err != nil {
			return schema.Expr{}, err
		}
		return schema.Expr{Kind: schema.ExprCol, Col: c.Name, Type: c.Type,
			Nullable: !c.NotNull}, nil
	}
	return schema.Expr{}, fmt.Errorf(
		"a branch filter takes this branch's columns and literals; " +
			"expressions and aggregates are not in scope before the merge")
}

// column resolves a field pointer against this branch's local instance.
func (b *UnionBranchSpec) column(fieldPtr any) (*schema.Column, error) {
	if fieldPtr == nil {
		return nil, fmt.Errorf("nil field pointer")
	}
	v := reflect.ValueOf(fieldPtr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil, fmt.Errorf("wants a field pointer like &m.Field, got %T", fieldPtr)
	}
	got := v.Pointer()
	if got < b.base || got >= b.base+b.size {
		return nil, fmt.Errorf(
			"the field pointer does not point into this branch's model — take " +
				"pointers into the local variable this branch was started with")
	}
	c, ok := b.tbl.off[got-b.base]
	if !ok {
		return nil, fmt.Errorf("no column at field offset %d", got-b.base)
	}
	return c.sc, nil
}

func (s *UnionSpec) fail(format string, a ...any) {
	s.b.errs.add(fmt.Errorf("union %q "+format, append([]any{s.u.Name}, a...)...))
	s.dead = true
}

// callUnion runs a union declaration against the finished tables and validates
// the result.
func (b *builder) callUnion(d *UnionDecl) {
	if !isExportedIdent(d.name) {
		b.errs.add(fmt.Errorf(
			"union name %q must be a valid exported Go identifier — it becomes a type name",
			d.name))
		return
	}
	for _, u := range b.outSch.Unions {
		if u.Name == d.name {
			b.errs.add(fmt.Errorf("two unions are named %q", d.name))
			return
		}
	}
	s := &UnionSpec{b: b, u: &schema.Union{Name: d.name}}
	if d.fn != nil {
		d.fn(s)
	}
	if s.dead {
		return
	}
	if !s.validate() {
		return
	}
	b.outSch.Unions = append(b.outSch.Unions, s.u)
}

// validate checks what the branch-by-branch rules could not.
func (s *UnionSpec) validate() bool {
	u := s.u
	if len(u.Branches) < 2 {
		s.fail("has %d branch(es) — a union of one is the query it wraps, "+
			"so declare that instead", len(u.Branches))
		return false
	}
	for i := range u.Branches {
		if got, want := len(u.Branches[i].Exprs), len(u.Cols); got != want {
			s.fail("branch %d projects %d column(s) and the first projects %d",
				i+1, got, want)
			return false
		}
	}
	if len(u.Cols) == 0 {
		s.fail("projects nothing")
		return false
	}
	// The ordering is the whole reason a union is one round trip rather than
	// several: without it the caller merges and sorts in Go, and paging is not
	// expressible at all.
	if len(u.OrderBy) == 0 {
		s.fail("declares no ordering — a merged feed with no order is a bag of " +
			"rows from several tables, and Limit over it returns an arbitrary " +
			"subset that can differ between runs")
		return false
	}
	for _, o := range u.OrderBy {
		if u.Col(o.Col) == nil {
			s.fail("orders by %q, which is not one of its output columns — "+
				"after the merge the branches' own columns are gone, so an "+
				"ordering may only name something every branch projected", o.Col)
			return false
		}
	}
	return true
}
