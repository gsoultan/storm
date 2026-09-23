// Package oratool is the Oracle half of the storm CLI.
//
// A package of its own for the reason tool/mstool is one, written down in
// scripts/check/coverage.sh before it was true of either: a target whose tests
// need a server it does not have lowers what the main CI job can measure, and
// the floor gets nudged down to follow. Splitting the code splits the
// measurement.
//
// It is SMALLER than mstool, and the difference is the port's second row shape.
// SQL Server needed a storm-written client, so mstool holds a raw-query
// validator and a dialer that builds two connections. Oracle goes through
// database/sql, so a handle is a handle and runtime/sqldrv is the adapter —
// there is nothing here to connect differently.
package oratool

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/runtime/sqldrv"
	oraintro "github.com/gsoultan/storm/schema/oracle"
)

// ImportModel is `storm import` against Oracle.
//
// The same shape as the PostgreSQL and SQL Server ones and deliberately so:
// read the catalogue, emit a GO MODEL. Not the DDL — storm is model-first, so
// adopting an existing database means having a model to start from, and the
// DDL is already in the database.
//
// The DRIVER is the caller's. database/sql needs one registered, and this
// package registers none: an application that already imports go-ora keeps the
// version it chose, and `cmd/storm` is where the choice is made visible.
func ImportModel(ctx context.Context, driver, dsn, ns, modulePath string) ([]byte, error) {
	if dsn == "" {
		return nil, errors.New(
			"import reads a live database: pass -dsn oracle://user:pass@host:1521/FREEPDB1 " +
				"(or set $STORM_DSN)")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	if ns == "public" {
		// -schema defaults to PostgreSQL's namespace, and Oracle has no such
		// schema at all. The empty string is not a fallback here — it means
		// the CONNECTED USER, which is what an application's unqualified
		// names resolve against and therefore the right default.
		ns = ""
	}
	s, err := oraintro.Introspect(ctx, sqldrv.New(db), ns)
	if err != nil {
		return nil, err
	}
	return codegen.Model(s, codegen.ModelOptions{Package: "model", Import: modulePath})
}
