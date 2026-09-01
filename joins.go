package storm

import (
	"fmt"
	"reflect"

	"github.com/gsoultan/storm/schema"
)

// Named joins: one read that projects across tables.
//
//	func (o *Order) Joins(j *storm.Joins) {
//	    var c Customer
//	    j.Named("WithCustomer").
//	        Inner(&c, &o.Customer).            // the FK relation says how to join
//	        Take(&o.ID, "OrderID").
//	        Take(&o.Total, "Total").
//	        Take(&c.Email, "Email").
//	        OrderDesc(&o.PlacedAt)
//	}
//
//	rows, err := order.New().
//	    Where(order.PlacedAt.Gte(since)).      // call-site predicates still compose
//	    AllWithCustomer(ctx, ex)               // []order.WithCustomerRow
//
// **A join projects; it does not load entities.** The output is a flat row of
// scalars, because a join answers a question and materialising two entity types
// to answer it is the round-tripping this exists to avoid. When you want the
// entities, that is a Plan — one query per relation, no fan-out.
//
// The joined model is a LOCAL variable. Taking field pointers into it is what
// makes `&c.Email` a checked reference rather than a string, so a rename of
// Customer.Email is a compile error here too.
type Joins struct {
	t *Table
	b *builder
}

// Joiner is implemented by models that declare joins. Optional.
type Joiner interface {
	Joins(*Joins)
}

// JoinBuilder accumulates one declaration.
type JoinBuilder struct {
	j    *Joins
	join *schema.Join
	dead bool
	seen map[string]bool
	// scope maps a declared alias to the table it refers to, so a column can
	// be qualified and type-checked.
	scope map[string]*scopeEntry
	// bases lets a field pointer into a LOCAL joined instance be resolved: the
	// registered table's offset map, read from a different base address.
	bases []joinBase
	// nullable marks aliases reached through a LEFT join.
	nullable map[string]bool
}

type scopeEntry struct {
	table *schema.Table
	// cte is set when the alias names a WITH clause rather than a real table.
	cte *schema.CTE
	agg *schema.Aggregate
}

type joinBase struct {
	base  uintptr
	size  uintptr
	tbl   *Table
	alias string
}

// Named starts a join. The generated type is this name plus "Row".
func (j *Joins) Named(name string) *JoinBuilder {
	b := &JoinBuilder{
		j: j, join: &schema.Join{Name: name},
		seen: map[string]bool{}, scope: map[string]*scopeEntry{},
		nullable: map[string]bool{},
	}
	if !isExportedIdent(name) {
		j.t.errs.add(fmt.Errorf(
			"%s: join name %q must be a valid exported Go identifier — it becomes a type name",
			j.t.out.Name, name))
		b.dead = true
		return b
	}
	for _, ex := range j.t.out.Joins {
		if ex.Name == name {
			j.t.errs.add(fmt.Errorf("%s: join %q is declared twice", j.t.out.Name, name))
			b.dead = true
			return b
		}
	}
	// The declaring table is always in scope under its own name.
	b.scope[j.t.out.Name] = &scopeEntry{table: j.t.out}
	b.bases = append(b.bases, joinBase{
		base: uintptr(j.t.base), size: j.t.typ.Size(), tbl: j.t, alias: j.t.out.Name})
	j.t.out.Joins = append(j.t.out.Joins, b.join)
	return b
}

// With materialises a declared aggregation as a CTE.
//
//	var o Order
//	j.Named("VsSpend").
//	    With("spend", &o, "ByCustomer").
//	    Inner(&c, storm.OnCols("spend", "customer_id", &c.ID))
//
// One pass over the aggregated table, reused by the join, instead of a
// correlated subquery per row.
func (b *JoinBuilder) With(alias string, model any, aggregate string) *JoinBuilder {
	if b.dead {
		return b
	}
	mi := b.j.b.infoFor(model)
	if mi == nil {
		b.fail("With %q: %T is not a registered model", alias, model)
		return b
	}
	var agg *schema.Aggregate
	for _, a := range mi.tbl.out.Aggregates {
		if a.Name == aggregate {
			agg = a
		}
	}
	if agg == nil {
		b.fail("With %q: %s declares no aggregate %q", alias, mi.tbl.out.Name, aggregate)
		return b
	}
	if _, dup := b.scope[alias]; dup {
		b.fail("With %q: the alias is already in scope", alias)
		return b
	}
	cte := schema.CTE{Alias: alias, Table: mi.tbl.out.Name, Aggregate: aggregate}
	b.join.CTEs = append(b.join.CTEs, cte)
	b.scope[alias] = &scopeEntry{table: mi.tbl.out, cte: &cte, agg: agg}
	return b
}

// Inner and Left attach a table.
//
// `on` is either a relation field pointer on the declaring model — the FK says
// how to join, so there is nothing to spell — or an explicit condition built
// with storm.OnCols.
func (b *JoinBuilder) Inner(model any, on any) *JoinBuilder {
	return b.attach(schema.JoinInner, model, on)
}

// Left keeps every row of the left side. Every column taken from the right
// becomes nullable in the generated row, which is what a LEFT JOIN means.
func (b *JoinBuilder) Left(model any, on any) *JoinBuilder {
	return b.attach(schema.JoinLeft, model, on)
}

// InnerWith and LeftWith attach a CTE that With() put in scope.
//
// With() materialises it; these say how it joins. Two steps because a CTE can
// be referenced by more than one join condition, and because "declare it" and
// "attach it" are genuinely different decisions.
func (b *JoinBuilder) InnerWith(alias string, on JoinOn) *JoinBuilder {
	return b.attachCTE(schema.JoinInner, alias, on)
}

// LeftWith keeps rows with no matching CTE row. Everything taken from the CTE
// becomes nullable, which is what a LEFT join means — and for an aggregate CTE
// it is usually the right choice, because a customer with no orders has no row
// in a GROUP BY over orders.
func (b *JoinBuilder) LeftWith(alias string, on JoinOn) *JoinBuilder {
	return b.attachCTE(schema.JoinLeft, alias, on)
}

func (b *JoinBuilder) attachCTE(kind schema.JoinKind, alias string, on JoinOn) *JoinBuilder {
	if b.dead {
		return b
	}
	sc, ok := b.scope[alias]
	if !ok || sc.cte == nil {
		b.fail("joins %q, which is not a CTE in scope — declare it with With(...) first", alias)
		return b
	}
	for _, t := range b.join.Tables {
		if t.Alias == alias {
			b.fail("joins the CTE %q twice", alias)
			return b
		}
	}
	if kind == schema.JoinLeft {
		b.nullable[alias] = true
	}
	cond, err := b.resolveJoinOn(on)
	if err != nil {
		b.fail("joining %q: %w", alias, err)
		return b
	}
	b.join.Tables = append(b.join.Tables, schema.JoinTable{Kind: kind, Alias: alias, On: cond})
	return b
}

func (b *JoinBuilder) attach(kind schema.JoinKind, model any, on any) *JoinBuilder {
	if b.dead {
		return b
	}
	mi := b.j.b.infoFor(model)
	if mi == nil {
		b.fail("joins %T, which is not a registered model", model)
		return b
	}
	alias := mi.tbl.out.Name
	if _, dup := b.scope[alias]; dup {
		b.fail("joins %s twice; storm does not alias a table to itself yet", alias)
		return b
	}
	// Register the LOCAL instance so field pointers into it resolve.
	b.bases = append(b.bases, joinBase{
		base:  reflect.ValueOf(model).Pointer(),
		size:  reflect.TypeOf(model).Elem().Size(),
		tbl:   mi.tbl,
		alias: alias,
	})
	b.scope[alias] = &scopeEntry{table: mi.tbl.out}
	if kind == schema.JoinLeft {
		b.nullable[alias] = true
	}

	cond, err := b.joinCond(alias, mi.tbl.out, on)
	if err != nil {
		b.fail("joining %s: %w", alias, err)
		return b
	}
	b.join.Tables = append(b.join.Tables, schema.JoinTable{
		Kind: kind, Table: mi.tbl.out.Name, Alias: alias, On: cond})
	return b
}

// joinCond turns the `on` argument into a condition.
func (b *JoinBuilder) joinCond(alias string, target *schema.Table, on any) (schema.Cond, error) {
	if c, ok := on.(JoinOn); ok {
		return b.resolveJoinOn(c)
	}
	// A relation field pointer: find the foreign key it names and build the
	// equality from it. This is the case worth having — the schema already
	// knows how these two tables relate, so making the developer restate it is
	// an invitation to state it wrongly.
	c, err := b.j.t.resolve(on)
	if err != nil {
		return schema.Cond{}, fmt.Errorf(
			"wants a relation field pointer like &m.Customer, or storm.OnCols(...): %w", err)
	}
	fk := b.j.t.fkFor(c.sc.Name)
	if fk == nil || fk.RefTable != target.Name {
		return schema.Cond{}, fmt.Errorf(
			"%s is not a foreign key to %s — use storm.OnCols(...) to say how they join",
			c.sc.Name, target.Name)
	}
	return schema.Cond{
		Kind: schema.CondCmp, Op: schema.OpEq,
		Left:  schema.Expr{Kind: schema.ExprCol, Tbl: b.j.t.out.Name, Col: c.sc.Name, Type: c.sc.Type},
		Right: schema.Expr{Kind: schema.ExprCol, Tbl: alias, Col: fk.RefColumns[0]},
	}, nil
}

// JoinOn is an explicit join condition.
type JoinOn struct {
	leftAlias string
	leftCol   string
	rightFP   any
	err       error
}

// OnCols joins on a named column of an aliased scope — a CTE's output column,
// or a table's column — against a field of the model being attached.
func OnCols(alias, column string, fieldPtr any) JoinOn {
	return JoinOn{leftAlias: alias, leftCol: column, rightFP: fieldPtr}
}

func (b *JoinBuilder) resolveJoinOn(c JoinOn) (schema.Cond, error) {
	if c.err != nil {
		return schema.Cond{}, c.err
	}
	sc, ok := b.scope[c.leftAlias]
	if !ok {
		return schema.Cond{}, fmt.Errorf("%q is not in scope", c.leftAlias)
	}
	col, ty, err := b.scopeColumn(sc, c.leftAlias, c.leftCol)
	if err != nil {
		return schema.Cond{}, err
	}
	rcol, ralias, rty, err := b.resolveAcross(c.rightFP)
	if err != nil {
		return schema.Cond{}, err
	}
	return schema.Cond{
		Kind: schema.CondCmp, Op: schema.OpEq,
		Left:  schema.Expr{Kind: schema.ExprCol, Tbl: c.leftAlias, Col: col, Type: ty},
		Right: schema.Expr{Kind: schema.ExprCol, Tbl: ralias, Col: rcol, Type: rty},
	}, nil
}

// scopeColumn finds a column in an alias's scope. A CTE's columns are its
// aggregation's outputs, not the underlying table's.
func (b *JoinBuilder) scopeColumn(sc *scopeEntry, alias, name string) (string, schema.Type, error) {
	if sc.agg != nil {
		for _, g := range sc.agg.By {
			if colEq(g.As, name) {
				return colName(g.As), g.Expr.Type, nil
			}
		}
		for _, t := range sc.agg.Terms {
			if colEq(t.As, name) {
				return colName(t.As), t.Expr.Type, nil
			}
		}
		return "", schema.Type{}, fmt.Errorf(
			"%s.%s: the CTE's aggregate has no output called that", alias, name)
	}
	if c := sc.table.Column(name); c != nil {
		return c.Name, c.Type, nil
	}
	return "", schema.Type{}, fmt.Errorf("%s has no column %s", alias, name)
}

// ---- projection -------------------------------------------------------------

// Take adds a column to the output. The field pointer may be into the
// declaring model or into any joined one.
func (b *JoinBuilder) Take(fieldPtr any, as string) *JoinBuilder {
	if b.dead {
		return b
	}
	if !isExportedIdent(as) {
		b.fail("output %q must be a valid exported Go identifier", as)
		return b
	}
	col, alias, ty, err := b.resolveAcross(fieldPtr)
	if err != nil {
		b.fail("%q: %w", as, err)
		return b
	}
	nullable := b.nullable[alias]
	if c := b.scope[alias].table.Column(col); c != nil && !c.NotNull {
		nullable = true
	}
	return b.push(as, schema.Expr{Kind: schema.ExprCol, Tbl: alias, Col: col, Type: ty}, ty, nullable)
}

// TakeFrom adds a column from an aliased scope — a CTE's aggregate output.
func (b *JoinBuilder) TakeFrom(alias, column, as string) *JoinBuilder {
	if b.dead {
		return b
	}
	if !isExportedIdent(as) {
		b.fail("output %q must be a valid exported Go identifier", as)
		return b
	}
	sc, ok := b.scope[alias]
	if !ok {
		b.fail("%q: %s is not in scope", as, alias)
		return b
	}
	col, ty, err := b.scopeColumn(sc, alias, column)
	if err != nil {
		b.fail("%q: %w", as, err)
		return b
	}
	// A CTE joined with LEFT, or an aggregate output that is itself nullable.
	nullable := b.nullable[alias]
	if sc.agg != nil {
		for _, t := range sc.agg.Terms {
			if colEq(t.As, column) && t.Expr.Nullable {
				nullable = true
			}
		}
	}
	return b.push(as, schema.Expr{Kind: schema.ExprCol, Tbl: alias, Col: col, Type: ty}, ty, nullable)
}

func (b *JoinBuilder) push(as string, e schema.Expr, ty schema.Type, nullable bool) *JoinBuilder {
	if b.seen[as] {
		b.fail("has two outputs named %q — each becomes a field of the same struct", as)
		return b
	}
	b.seen[as] = true
	b.join.Select = append(b.join.Select, schema.JoinCol{
		Expr: e, As: as, Type: ty, Nullable: nullable})
	return b
}

// ---- ordering and filtering -------------------------------------------------

// OrderAsc and OrderDesc order the joined result.
//
// A join has no natural order, and an unordered multi-table result shuffles
// between requests — the same reason an aggregation orders by its grouping.
func (b *JoinBuilder) OrderAsc(fieldPtr any) *JoinBuilder  { return b.order(fieldPtr, false) }
func (b *JoinBuilder) OrderDesc(fieldPtr any) *JoinBuilder { return b.order(fieldPtr, true) }

func (b *JoinBuilder) order(fieldPtr any, desc bool) *JoinBuilder {
	if b.dead {
		return b
	}
	col, alias, ty, err := b.resolveAcross(fieldPtr)
	if err != nil {
		b.fail("order: %w", err)
		return b
	}
	b.join.OrderBy = append(b.join.OrderBy, schema.JoinOrder{
		Expr: schema.Expr{Kind: schema.ExprCol, Tbl: alias, Col: col, Type: ty}, Desc: desc})
	return b
}

// Where declares a predicate the caller cannot widen. Call-site predicates
// still compose and are ANDed with it.
func (b *JoinBuilder) Where(c Cond) *JoinBuilder {
	if b.dead {
		return b
	}
	if b.join.Where != nil {
		b.fail("declares Where twice — combine them with storm.And")
		return b
	}
	sc, err := b.resolveCondAcross(c)
	if err != nil {
		b.fail("where: %w", err)
		return b
	}
	b.join.Where = &sc
	return b
}

// ---- cross-table resolution -------------------------------------------------

// resolveAcross finds which joined model a field pointer points into.
//
// Every registered instance occupies a distinct address range, so the pointer
// itself says which table it belongs to. That is what lets a join be declared
// with plain field pointers into local variables instead of strings.
func (b *JoinBuilder) resolveAcross(fieldPtr any) (col, alias string, ty schema.Type, err error) {
	if fieldPtr == nil {
		return "", "", schema.Type{}, fmt.Errorf("nil field pointer")
	}
	v := reflect.ValueOf(fieldPtr)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return "", "", schema.Type{}, fmt.Errorf("wants a field pointer like &m.Field, got %T", fieldPtr)
	}
	got := v.Pointer()
	for _, jb := range b.bases {
		if got < jb.base || got >= jb.base+jb.size {
			continue
		}
		c, ok := jb.tbl.off[got-jb.base]
		if !ok {
			return "", "", schema.Type{}, fmt.Errorf(
				"no column at field offset %d of %s", got-jb.base, jb.alias)
		}
		return c.sc.Name, jb.alias, c.sc.Type, nil
	}
	return "", "", schema.Type{}, fmt.Errorf(
		"the field pointer does not point into this model or any joined one — " +
			"declare the joined model as a local variable and take pointers into that")
}

func (b *JoinBuilder) resolveCondAcross(c Cond) (schema.Cond, error) {
	switch c.kind {
	case schema.CondCmp:
		l, err := b.termAcross(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		r, err := b.termAcross(c.right)
		if err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: schema.CondCmp, Op: c.op, Left: l, Right: r}, nil
	case schema.CondIsNull, schema.CondIsNotNull:
		l, err := b.termAcross(c.left)
		if err != nil {
			return schema.Cond{}, err
		}
		return schema.Cond{Kind: c.kind, Left: l}, nil
	default:
		args := make([]schema.Cond, 0, len(c.args))
		for _, a := range c.args {
			sa, err := b.resolveCondAcross(a)
			if err != nil {
				return schema.Cond{}, err
			}
			args = append(args, sa)
		}
		return schema.Cond{Kind: c.kind, Args: args}, nil
	}
}

func (b *JoinBuilder) termAcross(t Term) (schema.Expr, error) {
	if t.err != nil {
		return schema.Expr{}, t.err
	}
	switch t.kind {
	case schema.ExprLit:
		return schema.Expr{Kind: schema.ExprLit, Lit: t.lit,
			Type: schema.Type{Name: t.lit.Kind}}, nil
	case schema.ExprCol:
		col, alias, ty, err := b.resolveAcross(t.fp)
		if err != nil {
			return schema.Expr{}, err
		}
		return schema.Expr{Kind: schema.ExprCol, Tbl: alias, Col: col, Type: ty}, nil
	}
	return schema.Expr{}, fmt.Errorf("only columns and literals are supported in a join predicate")
}

func (b *JoinBuilder) fail(format string, a ...any) {
	b.j.t.errs.add(fmt.Errorf("%s: join %q "+format,
		append([]any{b.j.t.out.Name, b.join.Name}, a...)...))
	b.dead = true
}

// colEq matches a CTE output by its Go name or its column name, so either
// spelling works at the call site.
func colEq(as, want string) bool { return as == want || colName(as) == want }

// colName mirrors the alias the SQL back end derives from a declared field
// name. Kept here rather than imported so the root package stays free of
// compile/, which is a one-way dependency.
func colName(field string) string {
	var out []byte
	for i := 0; i < len(field); i++ {
		c := field[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && (isLowerByte(field[i-1]) || (i+1 < len(field) && isLowerByte(field[i+1]))) {
				out = append(out, '_')
			}
			out = append(out, c-'A'+'a')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func isLowerByte(c byte) bool { return c >= 'a' && c <= 'z' }
