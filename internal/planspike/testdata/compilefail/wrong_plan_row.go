// want: cannot use
//
// A plan's row type is not interchangeable with the bare row type. Assigning
// one to the other would let a caller pass an unloaded row where a loaded one
// is required, which is the same bug one indirection later.
package compilefail

import (
	"github.com/gsoultan/raorm/internal/planspike"
	"github.com/gsoultan/raorm/internal/planspike/gen/org"
)

func SubstitutePlanRow(r org.Row) planspike.OrgWithUsers {
	return r
}
