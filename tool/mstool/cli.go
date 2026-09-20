package mstool

import (
	"context"
	"errors"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/msdrv"
	msintro "github.com/gsoultan/storm/schema/mssql"
)

// ImportModel is `storm import` against SQL Server.
//
// The same shape as the PostgreSQL one and deliberately so: read the
// catalogue, emit a GO MODEL. Not the DDL — storm is model-first, so adopting
// an existing database means having a model to start from, and the DDL is
// already in the database.
func ImportModel(dsn, ns, modulePath string) ([]byte, error) {
	if dsn == "" {
		return nil, errors.New(
			"import reads a live database: pass -dsn sqlserver://user:pass@host:1433?database=... " +
				"(or set $STORM_DSN)")
	}
	cfg, err := msdrv.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	c, err := msdrv.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if ns == "public" {
		// -schema defaults to PostgreSQL's namespace, which is not a SQL Server
		// convention at all — there is no "public" schema here, so the default
		// would import nothing and say nothing. A caller who names one gets the
		// one they named.
		ns = "dbo"
	}
	s, err := msintro.Introspect(ctx, c, ns)
	if err != nil {
		return nil, err
	}
	src, err := codegen.Model(s, codegen.ModelOptions{Package: "model", Import: modulePath})
	if err != nil {
		return nil, err
	}
	return src, nil
}

// Dialer turns one DSN into the two-connection dialer normalisation
// needs. A SQL Server session is bound to its database at login, so the
// scratch database cannot be reached from the connection that found the
// target — see migrate/normalize_mssql.go.
func Dialer(dsn string) (migrate.MSSQLDialer, error) {
	if dsn == "" {
		return nil, errors.New("this reads a live database: pass " +
			"-dsn sqlserver://user:pass@host:1433?database=... (or set $STORM_DSN)")
	}
	base, err := msdrv.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, database string) (migrate.MSSQLConn, func(), error) {
		cfg := base
		if database != "" {
			cfg.Database = database
		}
		c, err := msdrv.Open(ctx, cfg)
		if err != nil {
			return nil, func() {}, err
		}
		return c, func() { c.Close() }, nil
	}, nil
}
