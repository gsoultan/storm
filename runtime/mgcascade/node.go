package mgcascade

// Node is one collection in a Plan and the references INTO it.
type Node struct {
	// Coll is the collection's name.
	Coll string
	// Edges are the references other collections hold to this one.
	Edges []Edge
}
