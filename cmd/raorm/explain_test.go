package main

import (
	"encoding/json"
	"testing"
)

// The walker is where a JSON shape mistake would silently pass everything, so
// it gets deterministic fixtures rather than whatever the CI planner produces.
func TestWalkPlan(t *testing.T) {
	plan := json.RawMessage(`{
		"Node Type": "Limit",
		"Plan Rows": 50,
		"Plans": [{
			"Node Type": "Sort",
			"Plan Rows": 50000,
			"Plans": [{
				"Node Type": "Seq Scan",
				"Relation Name": "users",
				"Plan Rows": 50000
			}]
		}, {
			"Node Type": "Index Scan",
			"Relation Name": "orgs",
			"Plan Rows": 90000
		}]
	}`)

	got := walkPlan(plan, 10000)
	if len(got) != 1 {
		t.Fatalf("flagged %d scans, want 1 — the nested seq scan and only it", len(got))
	}
	if got[0].table != "users" || got[0].rows != 50000 {
		t.Errorf("flagged %+v", got[0])
	}

	// Below the threshold: the planner is allowed to seq-scan small tables,
	// and on a CI database it always will. Flagging that is noise, not signal.
	if got := walkPlan(plan, 60000); len(got) != 0 {
		t.Errorf("a scan under the threshold was flagged: %+v", got)
	}
}
