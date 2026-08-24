// want: Eq undefined
//
// An array column offers no value predicates. Equality on an array is
// order-sensitive — ARRAY['a','b'] <> ARRAY['b','a'] — which is almost never
// what someone filtering by tag means, and containment and overlap need @> and
// &&, which raorm does not have yet. Absent beats surprising.
package compilefail

import (
	"github.com/gsoultan/raorm/internal/planspike/store/user"
)

func FilterOnArrayEquality() user.Query {
	return user.New().Where(user.Scopes.Eq([]string{"read"}))
}
