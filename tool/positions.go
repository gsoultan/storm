package tool

// Where a model was declared.
//
// storm.Build works from reflection and reflection has no source positions, so
// the schema it returns knows a table's Go type name and nothing about the file
// that declared it. The tool runs inside the adopter's module, where the source
// IS available — it already parses it to find models when there is no bootstrap
// — so it reads the positions back and hands them to the schema.
//
// Everything here is BEST EFFORT. A module that will not parse, a model in a
// package discovery does not reach, a column whose name does not invert to its
// field: each of those simply leaves the position empty and every message says
// exactly what it said before. A portability refusal is the wrong place to
// learn that a directory could not be read.

import (
	"github.com/gsoultan/storm/schema"
	"github.com/gsoultan/storm/tool/discover"
)

// annotate fills in Table.Pos and Column.Pos from the module's source.
func annotate(s *schema.Schema) {
	if s == nil {
		return
	}
	r, err := tooldiscover.Discover(".")
	if err != nil || r == nil {
		return
	}
	byType := make(map[string]tooldiscover.Model, len(r.Models))
	for _, m := range r.Models {
		// First wins. A type name repeated across packages is ambiguous, and
		// an ambiguous line is worse than none: it sends a reader to the wrong
		// file with confidence.
		if _, ok := byType[m.TypeName]; !ok {
			byType[m.TypeName] = m
		}
	}
	for _, t := range s.Tables {
		m, ok := byType[t.GoName]
		if !ok {
			continue
		}
		t.Pos = m.Pos
		for _, c := range t.Columns {
			// The field name is derived the same way codegen derives it, so a
			// column storm named from a field inverts; one renamed in the
			// schema does not, and is left without a position rather than
			// given a plausible wrong one.
			if p, ok := m.Fields[schema.GoName(c.Name)]; ok {
				c.Pos = p
			}
		}
	}
}
