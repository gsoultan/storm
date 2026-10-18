package storm_test

import "github.com/gsoultan/storm"

// rnVisit belongs to a person whose table is renamed; see
// TestForeignKeyFollowsTargetRename.
type rnVisit struct {
	storm.Model
	Person rnPerson
}
