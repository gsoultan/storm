package hooks

import "github.com/gsoultan/storm/runtime"

// valueRows is rows for a driver that decodes values (runtime.ValueRows), so
// wrapping a database/sql Executor does not change the shape its generated
// package reads.
type valueRows struct {
	*rows
	vals runtime.ValueRows
}

func (v *valueRows) Values() []any { return v.vals.Values() }
