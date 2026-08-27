// want: Posts undefined
//
// A plan's row type carries exactly the relations that plan declared. Summary
// names only Org, so reading Posts off it does not compile — the same
// guarantee as ADR-0003, one level up: not just "was this relation loaded" but
// "was it loaded BY THIS PLAN".
package compilefail

import (
	"github.com/gsoultan/storm/internal/planspike/store"
)

func ReadUndeclaredRelation(r store.UserSummaryRow) int {
	return len(r.Posts)
}
