package migrate

import (
	"context"
	"fmt"

	"github.com/gsoultan/storm/schema"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AutoPool is Auto against a pool, which is what an application actually holds.
//
// It acquires one connection and holds it for the whole migration. That is not
// an optimisation: the advisory lock serialising migrating processes is
// SESSION-scoped, so a migration spread across pooled connections would take
// the lock on one session and apply the DDL on another — which is to say, not
// hold it at all.
//
//	if _, err := migrate.AutoPool(ctx, pool, s, migrate.AutoOptions{
//		Logf: log.Printf,
//	}); err != nil {
//		return err
//	}
//
// where s came from storm.Build(model.All()...).
func AutoPool(ctx context.Context, p *pgxpool.Pool, want *schema.Schema, o AutoOptions) (Result, error) {
	c, err := p.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire a connection to migrate on: %w", err)
	}
	defer c.Release()
	return Auto(ctx, c.Conn(), want, o)
}
