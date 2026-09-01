package schema

// JoinKind is how a table is attached.
type JoinKind uint8

const (
	// JoinInner drops rows with no match on either side.
	JoinInner JoinKind = iota
	// JoinLeft keeps every row of the left side. Every column taken from the
	// right becomes NULLABLE because of it — that is what a LEFT JOIN means,
	// and typing those columns as non-null would decode a missing match as a
	// zero value.
	JoinLeft
)

// Join is one named read that projects across tables.
//
// The output is a flat row of scalars, not a graph of entities: a join answers
// a question ("orders with the customer's email"), and materialising two entity
// types to answer it is the round-tripping this avoids. A relation LOAD is the
// other tool — see Plan — and it is the right one when you want the entities.
type Join struct {
	// Name is as declared. The generated type is this name plus "Row".
	Name string

	// CTEs are the WITH clauses, in order. Each is a declared aggregation on
	// some table, materialised once and joined against.
	CTEs []CTE

	// Tables are the joined relations, in the order they attach. The declaring
	// table is the FROM and is not listed.
	Tables []JoinTable

	// Select is the output, in declaration order.
	Select []JoinCol

	// Where is a declared predicate over the joined shape. Call-site
	// predicates still compose on top and apply to the declaring table.
	Where *Cond

	// OrderBy is the declared ordering. A join has no natural one, and an
	// unordered multi-table result shuffles between requests.
	OrderBy []JoinOrder
}

// CTE is a WITH clause: a declared aggregation, materialised under an alias.
type CTE struct {
	// Alias names it in the query.
	Alias string
	// Table and Aggregate name the declared aggregation this CTE runs.
	Table     string
	Aggregate string
}

// JoinTable is one attached table.
type JoinTable struct {
	Kind JoinKind
	// Table is the joined table's name. Empty when Alias names a CTE.
	Table string
	// Alias is how the table is referred to in the query. For a plain join it
	// is the table name; for a CTE it is the CTE's alias.
	Alias string
	// On is the join condition.
	On Cond
}

// JoinCol is one output column.
type JoinCol struct {
	Expr Expr
	As   string
	// Nullable is the resolved nullability, which is the column's own OR'd
	// with whether it came through a LEFT join.
	Nullable bool
	// Type is the resolved type.
	Type Type
}

// JoinOrder is one ORDER BY term.
type JoinOrder struct {
	Expr Expr
	Desc bool
}
