// want: Eq undefined
//
// A jsonb column offers no value predicates. Whole-document equality is almost
// never what a caller means — `{"a":1,"b":2}` and `{"b":2,"a":1}` compare
// unequal as text and equal as jsonb, and neither is what someone filtering on
// a field wanted. Content filtering needs ->> and @>, which raorm does not have
// yet, so the operator is absent rather than surprising.
package compilefail

import (
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	"github.com/gsoultan/raorm/runtime"
)

func FilterOnJSONBEquality() user.Query {
	return user.New().Where(user.Prefs.Eq(runtime.JSON(`{}`)))
}
