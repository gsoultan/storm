package codegen

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Plan costing — the analysis half of `storm lint --plans`.
//
// A named plan's round-trip cost is knowable at generate time: one for the
// parents, one per relation, one per nested relation. That is the whole reason
// plans.go exists as a reviewable file — every load pattern in the system, each
// with a cost a reviewer can read off — and lint is what turns "can read off"
// into "is checked in CI".
//
// The count is the WORST case. A level with nothing to fetch skips its query
// at run time, so the real cost is often lower; lint budgets for the day it is
// not.

// PlanCost is one named plan's worst-case round trips.
type PlanCost struct {
	Name       string
	Table      string
	RoundTrips int
	// Chain renders the loads in issue order, e.g.
	// "users → posts (= ANY) → comments (= ANY); → org (= ANY)".
	Chain string
}

// PlanCosts computes every named plan's cost for the given tables.
func PlanCosts(s *schema.Schema, tables []string) ([]PlanCost, error) {
	named, err := namedPlansFor(s, tables)
	if err != nil {
		return nil, err
	}
	out := make([]PlanCost, 0, len(named))
	for _, np := range named {
		c := PlanCost{Name: np.Name, Table: np.parent.Name, RoundTrips: 1}
		var parts []string
		for _, m := range np.members {
			c.RoundTrips += 1 + countNested(m.Nested)
			parts = append(parts, renderChain(m))
		}
		c.Chain = np.parent.Name + " → " + strings.Join(parts, "; → ")
		out = append(out, c)
	}
	return out, nil
}

func countNested(ms []planMemberT) int {
	n := 0
	for _, m := range ms {
		n += 1 + countNested(m.Nested)
	}
	return n
}

func renderChain(m planMemberT) string {
	// Every batch loader is the two-query `= ANY($1)` mechanism; saying so per
	// hop keeps the output honest when a LATERAL or join strategy exists.
	s := fmt.Sprintf("%s (= ANY)", m.child.Name)
	for _, sub := range m.Nested {
		s += " → " + renderChain(sub)
	}
	return s
}
