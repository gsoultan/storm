package schema

// Match is a declared probe on matching columns: the rows of Target whose
// TargetColumns equal this table's Columns, pair by pair. It is the
// semi-join a foreign key does not describe — two tables that share an
// instance and a step, say — declared once so the composers that ask it are
// generated like a relation's.
type Match struct {
	// Name is the declaration's name, which the composers are named after:
	// <Model>Having<Name>, <Model>NotHaving<Name>.
	Name string

	// Target is the table probed, and TargetGo its model type name.
	Target   string
	TargetGo string

	// Columns are this table's columns, TargetColumns the target's, in pairs.
	Columns       []string
	TargetColumns []string
}
