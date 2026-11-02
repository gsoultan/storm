package mgcascade

// Edge is one reference: a child collection's fields that hold a parent's
// values, and the action its declaration names.
type Edge struct {
	// Child is the referencing collection's index in the Plan.
	Child int
	// Name is the reference as the model spells it, for an error:
	// "books.author_id".
	Name string
	// From are the child's stored fields; To the parent's, pairwise.
	From, To []string
	Action   Action
	// Set is the pipeline stage SetNull and SetDefault apply to a child:
	// [{"$set":{...}}], written at generate time.
	Set string
}
