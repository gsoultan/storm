package codegen

import (
	"fmt"
	"sort"

	"github.com/gsoultan/raorm/schema"
)

// FlushOrder ranks tables so that every table's foreign-key targets rank
// strictly lower — a write ordered by this rank satisfies its references
// without the database deferring anything.
//
// Self-references are ignored. A table referencing itself constrains the order
// of *rows*, not of tables, and treating it as a cycle would make every
// hierarchy unwritable.
//
// A genuine cycle between two tables is an error, and it names the cycle. A
// mutual reference cannot be written in any order without deferring a
// constraint, which is a modelling decision the author has to make deliberately
// — the usual fix is to make one side nullable and write it in two steps.
func FlushOrder(s *schema.Schema) (map[string]int, error) {
	deps := make(map[string]map[string]bool, len(s.Tables))
	names := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		names = append(names, t.Name)
		deps[t.Name] = map[string]bool{}
	}
	sort.Strings(names) // determinism does not depend on declaration order

	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			if fk.RefTable == t.Name {
				continue // self-reference: orders rows, not tables
			}
			if _, known := deps[fk.RefTable]; !known {
				// A reference out of this context cannot be ordered against it.
				// Not an error: the target is simply written elsewhere, and
				// pretending otherwise would block generating one context at a
				// time.
				continue
			}
			deps[t.Name][fk.RefTable] = true
		}
	}

	rank := make(map[string]int, len(names))
	// Kahn's algorithm over a stable name order, so the output is the same on
	// every machine — `raorm verify` fails CI on stale output and a map
	// iteration leaking in here would make it fail at random.
	for len(rank) < len(names) {
		progress := false
		for _, n := range names {
			if _, done := rank[n]; done {
				continue
			}
			ready := true
			for d := range deps[n] {
				if _, done := rank[d]; !done {
					ready = false
					break
				}
			}
			if ready {
				rank[n] = len(rank)
				progress = true
			}
		}
		if !progress {
			return nil, fmt.Errorf(
				"codegen: foreign keys form a cycle among %s — no write order satisfies "+
					"them all. Make one side nullable and write it in two steps",
				remaining(names, rank))
		}
	}
	return rank, nil
}

func remaining(names []string, rank map[string]int) []string {
	var out []string
	for _, n := range names {
		if _, done := rank[n]; !done {
			out = append(out, n)
		}
	}
	return out
}
