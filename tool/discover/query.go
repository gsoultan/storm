package tooldiscover

// Query is one storm.SQL[T] or storm.SQLExec declaration found in the
// adopter's source.
//
// These need discovering for the same reason models do: a raw query that is
// not registered gets no generated scanner, and the failure surfaces at the
// first call as "no scanner generated for T" rather than at generate time.
type Query struct {
	// ImportPath is the package the synthesized bootstrap must import.
	ImportPath string
	// PkgName is the package clause, which is the import's local name.
	PkgName string
	// VarName is the package-level variable holding the declaration.
	VarName string
	// Pos is file:line, for errors.
	Pos string
}
