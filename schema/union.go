package schema

// A declared UNION: several tables projected into one row shape.
//
// Unlike every other cross-table read, a union has no DRIVING table. A join
// hangs off the table that declares it and reads the far side; a feed of
// comments, follows and releases has no such centre — none of the three is the
// one the others attach to. So a Union is registered against the SCHEMA rather
// than a table, and its generated row type lives with the declaration instead
// of in some arbitrary table's package (ADR-0008).
type Union struct {
	// Name is as declared. The generated type is this name plus "Row".
	Name string

	// Branches are the SELECTs, in declaration order. That order is the order
	// they are rendered, which matters only for reading the SQL: the ORDER BY
	// applies to the union as a whole.
	Branches []UnionBranch

	// Cols is the shared output shape, in declaration order. Every branch
	// projects exactly these names, in this order — that is what makes the
	// branches union-compatible, and it is checked rather than assumed.
	Cols []UnionCol

	// OrderBy is the ordering over the WHOLE union. It may only name output
	// columns: a union's rows come from different tables and the sort happens
	// after they are merged, so there is nothing else in scope.
	OrderBy []UnionOrder

	// Params are the values the call supplies, in declaration order — which is
	// the order they appear in the generated function's signature.
	//
	// A union has no call-site predicates (its branches read different tables,
	// so a predicate would have to say which one it filtered), and a feed that
	// cannot be narrowed to one actor is not much of a feed. Declared
	// parameters are the narrow answer: the SHAPE stays fixed, and only values
	// vary.
	Params []UnionParam

	// Distinct selects UNION over UNION ALL.
	//
	// ALL is the default, inverting SQL's, because de-duplicating means
	// sorting or hashing the entire result before a single row is returned and
	// no feed wants that. A caller who does want it says so.
	Distinct bool
}

// UnionBranch is one SELECT of a union.
type UnionBranch struct {
	// Table is the table this branch reads.
	Table string

	// Exprs are the projected expressions, positionally matched to Union.Cols.
	Exprs []Expr

	// Where is the branch's declared filter, fixed at generate time like a
	// join's. Nil is no filter.
	Where *Cond
}

// UnionCol is one column of the shared output shape.
type UnionCol struct {
	// As is the Go field name in the generated row type.
	As string
	// Type is the resolved type, which every branch must agree on.
	Type Type
	// Nullable is true when ANY branch can produce NULL here. A column that is
	// NOT NULL in two tables and nullable in the third is nullable in the
	// union, and typing it otherwise would decode a NULL as a zero value.
	Nullable bool
}

// UnionParam is one declared parameter.
type UnionParam struct {
	// Name is the Go argument name in the generated function.
	Name string
	// Type is resolved from the column the parameter is first compared with:
	// a parameter has no type of its own, and inferring it from use means the
	// generated signature cannot disagree with the column it filters.
	Type Type
}

// UnionOrder is one ORDER BY term over the merged rows.
type UnionOrder struct {
	// Col is the output column name, an index into Union.Cols.
	Col string
	// Desc orders descending.
	Desc bool
	// NullsFirst is meaningful only when explicitly set.
	NullsFirst *bool
}

// Col finds an output column by name.
func (u *Union) Col(as string) *UnionCol {
	for i := range u.Cols {
		if u.Cols[i].As == as {
			return &u.Cols[i]
		}
	}
	return nil
}
