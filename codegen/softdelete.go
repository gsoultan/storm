package codegen

import "fmt"

// splice picks the runtime splice for a read: the plain one, or the form that
// ANDs a declared predicate in front of the caller's.
//
// Every read of a soft-delete table goes through here. That is the point — the
// hazard docs/CONCEPT.md names is "every query that FORGETS the predicate", and
// the way to make forgetting impossible is to have exactly one place that
// decides, rather than a rule each emitter has to remember. A new read path
// that calls runtime.SpliceTree directly is the bug this function exists to
// prevent, and TestNoUnguardedReadOnSoftDeleteTable is what notices.
func (g *gen) splice(prefix, rest string) string {
	if !g.t.SoftDeletes() {
		return fmt.Sprintf("runtime.SpliceTree(%s, %s)", prefix, rest)
	}
	return fmt.Sprintf("runtime.SpliceTreeWhere(%s, softDeleteWhere, %s)", prefix, rest)
}
