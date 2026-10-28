package pgstat

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/gsoultan/storm/runtime"
)

const (
	databaseSizeSQL = "SELECT pg_database_size(current_database())::text"

	tableSizesSQL = `SELECT c.relname::text, pg_total_relation_size(c.oid)::text
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p') AND n.nspname = $1
 ORDER BY c.relname`
)

// DatabaseSize is the bytes the current database takes on disk, as
// pg_database_size counts them: rows, indexes, TOAST and free space alike.
func DatabaseSize(ctx context.Context, ex runtime.Executor) (int64, error) {
	rows, err := ex.Query(ctx, databaseSizeSQL, nil)
	if err != nil {
		return 0, fmt.Errorf("pgstat: read the database size: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, errors.Join(errors.New("pgstat: the database size query returned no row"), rows.Err())
	}
	n, err := strconv.ParseInt(string(rows.RawValues()[0]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pgstat: read the database size: %w", err)
	}
	return n, rows.Err()
}

// TableSizes is the bytes each table of schema takes on disk, by name, as
// pg_total_relation_size counts them: with its indexes and TOAST.
func TableSizes(ctx context.Context, ex runtime.Executor, schema string) (map[string]int64, error) {
	rows, err := ex.Query(ctx, tableSizesSQL, []any{schema})
	if err != nil {
		return nil, fmt.Errorf("pgstat: read the table sizes: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		raw := rows.RawValues()
		n, err := strconv.ParseInt(string(raw[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pgstat: read the size of %s: %w", raw[0], err)
		}
		out[string(raw[0])] = n
	}
	return out, rows.Err()
}
