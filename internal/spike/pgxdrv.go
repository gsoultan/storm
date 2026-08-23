package spike

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxExec is the only file in the spike that knows pgx exists.
type PgxExec struct{ Pool *pgxpool.Pool }

func (e PgxExec) Query(ctx context.Context, sql string, args []any) (Rows, error) {
	r, err := e.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxRows{r}, nil
}

type pgxRows struct{ pgx.Rows }

func (r pgxRows) Close() { r.Rows.Close() }
