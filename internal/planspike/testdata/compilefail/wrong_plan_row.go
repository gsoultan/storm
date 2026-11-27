// want: cannot use
//
// A plan's row type is not interchangeable with the bare row type. Assigning
// one to the other would let a caller pass an unloaded row where a loaded one
// is required, which is the same bug one indirection later.
package compilefail

import (
	"github.com/gsoultan/storm/v2/internal/planspike/store"
	"github.com/gsoultan/storm/v2/internal/planspike/store/org"
)

func SubstitutePlanRow(r org.Row) store.OrgWithUsersRow {
	return r
}
