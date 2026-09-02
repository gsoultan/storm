package codegen

import (
	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// Statement enumeration for `storm explain`.
//
// The queries storm will issue are knowable from the model: every table's base
// read, every projection, every declared aggregation and join, and every named
// plan's component loads. Enumerating them here — from the same pgsql lowering
// the generator uses — means explain examines the statements production will
// run, not approximations of them.
//
// The declared aggregations and joins are the ones this gate is really for.
// A base read is a SELECT of known columns and it plans or the table is
// missing; an aggregation carries GROUP BY, HAVING, FILTER, grouping sets and
// window frames, and a join carries CTEs and an ON clause across tables. That
// is where storm can emit SQL that is well-formed to it and rejected by
// PostgreSQL, and until 2026-09-02 it was the part explain did not look at.

// ExplainQuery is one statement worth explaining.
type ExplainQuery struct {
	Label string // "users (base read)" or "UserFeed → posts"
	SQL   string
}

// ExplainQueries enumerates every statement the generated code can issue with
// no call-site predicates: the base read and each projection per table, each
// declared aggregation and join, and every named plan's loads.
//
// Predicates are omitted deliberately. They are spliced at run time from a
// bounded set of shapes, each already lowered by the same pgsql code and
// covered by the compiler fuzzer; what cannot be covered there is the fixed
// text, which is what this returns.
func ExplainQueries(s *schema.Schema, tables []string) ([]ExplainQuery, error) {
	var out []ExplainQuery

	for _, name := range tables {
		t := s.Table(name)
		if t == nil {
			continue
		}
		order := pgsql.OrderSuffix(pgsql.DefaultOrderBy(t.PrimaryKey, t.Columns[0].Name)) + "1"
		out = append(out, ExplainQuery{
			Label: t.Name + " (base read)",
			SQL:   pgsql.SelectPrefix(t.Name, readableCols(t)) + order,
		})
		for _, pr := range t.Projections {
			out = append(out, ExplainQuery{
				Label: t.Name + " (projection " + pr.Name + ")",
				SQL:   pgsql.SelectPrefix(t.Name, pr.Columns) + order,
			})
		}
		for _, a := range t.Aggregates {
			out = append(out, ExplainQuery{
				Label: t.Name + " → " + a.Name + " (aggregate)",
				SQL:   pgsql.AggregateSelect(t.Name, a) + pgsql.AggregateSuffix(a),
			})
		}
		for _, j := range t.Joins {
			sql, err := joinExplainSQL(s, t, j)
			if err != nil {
				return nil, err
			}
			out = append(out, ExplainQuery{Label: t.Name + " → " + j.Name + " (join)", SQL: sql})
		}
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

// joinExplainSQL assembles a declared join exactly as the generator does: the
// prefix (with any CTE bodies inlined), the declared WHERE the splice folds in,
// then the ORDER BY.
//
// A join with a declared WHERE has to be assembled here rather than being
// prefix+suffix, because the runtime puts call-site predicates BETWEEN them and
// concatenating the two directly would explain a statement with the WHERE
// keyword missing — a syntax error attributed to storm rather than to this
// function.
func joinExplainSQL(s *schema.Schema, t *schema.Table, j *schema.Join) (string, error) {
	var cteErr error
	prefix := pgsql.JoinSelect(t.Name, j, func(c schema.CTE) (string, string) {
		ct := s.Table(c.Table)
		if ct == nil {
			cteErr = &cteRefError{alias: c.Alias, table: c.Table}
			return "", ""
		}
		for _, a := range ct.Aggregates {
			if a.Name == c.Aggregate {
				return pgsql.AggregateSelect(ct.Name, a), pgsql.AggregateSuffix(a)
			}
		}
		cteErr = &cteRefError{alias: c.Alias, table: c.Table, agg: c.Aggregate}
		return "", ""
	})
	if cteErr != nil {
		return "", cteErr
	}
	sql := prefix
	if w := pgsql.JoinDeclaredWhere(j); w != "" {
		sql += pgsql.WhereLead + w
	}
	return sql + pgsql.JoinSuffix(j), nil
}

type cteRefError struct{ alias, table, agg string }

func (e *cteRefError) Error() string {
	if e.agg == "" {
		return "codegen: CTE " + e.alias + " names table " + e.table + ", which is not in this context"
	}
	return "codegen: CTE " + e.alias + " names aggregate " + e.table + "." + e.agg + ", which does not exist"
}
