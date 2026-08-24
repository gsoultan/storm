package codegen

import (
	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Statement enumeration for `raorm explain`.
//
// The queries raorm will issue are knowable from the model: every table's base
// read, and every named plan's component loads. Enumerating them here — from
// the same pgsql lowering the generator uses — means explain examines the
// statements production will run, not approximations of them.

// ExplainQuery is one statement worth explaining.
type ExplainQuery struct {
	Label string // "users (base read)" or "UserFeed → posts"
	SQL   string
}

// ExplainQueries enumerates the base read per table and every named plan's
// loads.
func ExplainQueries(s *schema.Schema, tables []string) ([]ExplainQuery, error) {
	var out []ExplainQuery

	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		out = append(out, ExplainQuery{
			Label: t.Name + " (base read)",
			SQL: pgsql.SelectPrefix(t.Name, readableCols(t)) +
				pgsql.OrderSuffix(pgsql.DefaultOrderBy(t.PrimaryKey, t.Columns[0].Name)) + "1",
		})
	}

	named, err := namedPlansFor(s, tables)
	if err != nil {
		return nil, err
	}
	for _, np := range named {
		for _, m := range np.members {
			q, err := memberQuery(m)
			if err != nil {
				return nil, err
			}
			out = append(out, ExplainQuery{Label: np.Name + " → " + m.child.Name, SQL: q})
			for _, sub := range m.Nested {
				q, err := memberQuery(sub)
				if err != nil {
					return nil, err
				}
				out = append(out, ExplainQuery{
					Label: np.Name + " → " + m.child.Name + " → " + sub.child.Name,
					SQL:   q,
				})
			}
		}
	}
	return out, nil
}

// memberQuery is the batch load one relation issues: the child's base read
// filtered by the key column. Every piece of SQL text comes from pgsql — this
// function assembles ordinals, which are digits, not dialect.
func memberQuery(m planMemberT) (string, error) {
	// The column the child query filters on: the child's FK for a has-many,
	// the child's primary key for a to-one.
	col := m.rel.Column
	if !m.rel.ToMany {
		col = m.ParentKey
	}
	a, b, ok := pgsql.Frag("In", pgsql.Ident(col))
	if !ok {
		return "", errNoLowering(m.child.Name, col)
	}
	return pgsql.SelectPrefix(m.child.Name, readableCols(m.child)) +
		pgsql.WhereLead + a + "1" + b +
		pgsql.OrderSuffix(pgsql.DefaultOrderBy(m.child.PrimaryKey, m.child.Columns[0].Name)) + "2", nil
}

func errNoLowering(table, col string) error {
	return &noLoweringError{table: table, col: col}
}

type noLoweringError struct{ table, col string }

func (e *noLoweringError) Error() string {
	return "codegen: no ANY lowering for " + e.table + "." + e.col
}
