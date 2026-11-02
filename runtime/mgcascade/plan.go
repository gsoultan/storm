package mgcascade

// Plan is every collection a cascade can reach from Nodes[0], the collection
// whose package carries it. Edges name their child by index, so a reference
// that loops — a tree's parent_id — is a plan of finite size.
type Plan struct {
	Nodes []Node
}
