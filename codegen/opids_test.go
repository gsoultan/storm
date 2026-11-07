package codegen

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// TestOpIdsMatchRuntime holds codegen's ops table to runtime's operator ids.
// Generated packages name runtime.Op<name>; the ids are what a token carries
// and what the bind path's ListOp and ArglessOp test, so the two lists must
// agree in order, in which operators take a list, and in which bind nothing.
func TestOpIdsMatchRuntime(t *testing.T) {
	want := []runtime.Op{
		runtime.OpEq, runtime.OpNotEq, runtime.OpGt, runtime.OpGte, runtime.OpLt, runtime.OpLte,
		runtime.OpEqLower, runtime.OpLike, runtime.OpILike, runtime.OpMatches, runtime.OpWebSearch,
		runtime.OpOverlaps, runtime.OpContainsRange, runtime.OpContainedBy, runtime.OpIn, runtime.OpNotIn,
		runtime.OpArrayContains, runtime.OpArrayContainedBy, runtime.OpArrayOverlaps,
		runtime.OpJSONContains, runtime.OpJSONContainedBy, runtime.OpHasAnyKey, runtime.OpHasAllKeys,
		runtime.OpJSONTextLikeAny, runtime.OpTextHasPrefix, runtime.OpTextHasSuffix, runtime.OpTextContains,
		runtime.OpIsNull, runtime.OpIsNotNull, runtime.OpJSONIsEmpty, runtime.OpNullOrEmpty,
	}
	if len(want) != len(ops) {
		t.Fatalf("codegen has %d operators, runtime %d", len(ops), len(want))
	}
	for i, op := range ops {
		if want[i] != runtime.Op(i+1) {
			t.Errorf("runtime id of %s is %d, codegen numbers it %d", op.name, want[i], i+1)
		}
		if runtime.ListOp(want[i]) != op.list() {
			t.Errorf("%s: runtime.ListOp = %v, codegen list() = %v", op.name, runtime.ListOp(want[i]), op.list())
		}
		if runtime.ArglessOp(want[i]) != (op.args == 0) {
			t.Errorf("%s: runtime.ArglessOp = %v, codegen args = %d", op.name, runtime.ArglessOp(want[i]), op.args)
		}
	}
	if runtime.OpExists != runtime.Op(len(ops)+1) || runtime.OpNotExists != runtime.Op(len(ops)+2) {
		t.Errorf("existence operators must follow the table: got %d, %d", runtime.OpExists, runtime.OpNotExists)
	}
}
