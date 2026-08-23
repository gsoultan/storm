// want: Users undefined
//
// ADR-0003 in one file. A relation that was not loaded is not an empty slice,
// not a lazy fetch and not a lint warning — the field does not exist.
package compilefail

import (
	"context"

	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/runtime"
)

func ReadUnloadedRelation(ctx context.Context, ex runtime.Executor) int {
	rows, err := org.New().All(ctx, ex, nil)
	if err != nil {
		return 0
	}
	// org.Row came from a plain query. It has no Users field, and never will.
	return len(rows[0].Users)
}
