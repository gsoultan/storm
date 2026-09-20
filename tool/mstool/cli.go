package mstool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/msdrv"
	"github.com/gsoultan/storm/schema"
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

// ReplayAndDiff applies migration files to a scratch database and returns what
// the model still wants that none of them carry.
//
// This is `storm verify -pending` for SQL Server, and it is a scratch DATABASE
// for the reason everything else here is: there is no search_path to point at a
// scratch schema, so a database is the only namespace that can be created and
// dropped without leaving the server different afterwards.
//
// files is already globbed and sorted by the caller, because deciding WHICH
// files are migrations — and refusing a directory whose .sql files storm would
// silently skip — is the same decision for every dialect.
func ReplayAndDiff(ctx context.Context, dsn string, files []string, model *schema.Schema) (migrate.Plan, error) {
	base, err := Dialer(dsn)
	if err != nil {
		return migrate.Plan{}, err
	}
	admin, closeAdmin, err := base(ctx, "master")
	if err != nil {
		return migrate.Plan{}, fmt.Errorf("connect to master to build the scratch database: %w", err)
	}
	defer closeAdmin()

	name := fmt.Sprintf("storm_pending_%d", os.Getpid())
	drop := func(ctx context.Context) {
		// SINGLE_USER first: a connection this process opened and closed can
		// still be lingering, and DROP DATABASE fails on one. Guarded on DB_ID
		// so this is also the "clean up after a crashed run" path.
		_, _ = admin.Exec(ctx, "IF DB_ID(N'"+name+"') IS NOT NULL BEGIN "+
			"ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
			"DROP DATABASE ["+name+"]; END", nil)
	}
	drop(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE ["+name+"]", nil); err != nil {
		return migrate.Plan{}, fmt.Errorf("create scratch database %s: %w\n"+
			"       the account needs CREATE DATABASE permission", name, err)
	}
	defer func() {
		// Detached from the caller's context: the reason this is unwinding may
		// be that the context is gone, and the scratch database has to go
		// either way.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
		defer cancel()
		drop(ctx)
	}()

	// A dialer whose "" is the replay database. "master" and the normalisation
	// database migrate builds under it pass straight through, which is what
	// lets ForMSSQL normalise the model while the migrations sit in here.
	scratch := func(ctx context.Context, db string) (migrate.MSSQLConn, func(), error) {
		if db == "" {
			db = name
		}
		return base(ctx, db)
	}
	if err := replay(ctx, scratch, files); err != nil {
		return migrate.Plan{}, err
	}
	return migrate.ForMSSQL(ctx, scratch, "dbo", model)
}

// replay applies each file as ONE batch.
//
// Per file rather than per statement, because that is what a migration runner
// does — and because a file storm wrote can need it: the guarded default drop
// is a DECLARE and three statements, and DECLARE is scoped to the BATCH. Split
// at the semicolons and the variable would be undefined by the line that reads
// it.
func replay(ctx context.Context, dial migrate.MSSQLDialer, files []string) error {
	c, closeC, err := dial(ctx, "")
	if err != nil {
		return err
	}
	defer closeC()
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := c.Exec(ctx, string(sql), nil); err != nil {
			return fmt.Errorf("replaying %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// dropTimeout bounds the scratch database's removal, which runs on a context
// detached from the caller's and so needs a deadline of its own.
const dropTimeout = 30 * time.Second
