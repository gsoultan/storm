// Package pgxdrv is the only package in raorm that knows pgx exists.
// No pgx type crosses out of it (AGENTS.md, CI-enforced).
package pgxdrv

import (
	"context"

	"github.com/gsoultan/raorm/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool adapts a pgxpool to runtime.Executor.
type Pool struct{ P *pgxpool.Pool }

func (e Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	r, err := e.P.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows{r}, nil
}

func (e Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.P.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type rows struct{ pgx.Rows }

func (r rows) Close() { r.Rows.Close() }

var _ runtime.Executor = Pool{}
