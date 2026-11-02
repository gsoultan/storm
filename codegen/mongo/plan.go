package codegenmongo

import "github.com/gsoultan/storm/schema"

// plan is a row type and the query that fills it: one per relation, and one
// per plan the model named.
type plan struct {
	name      string
	parent    *schema.Table
	parentPkg string
	members   []member
}
