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
	"fmt"
	"strings"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/runtime/sqldrv"
	oraintro "github.com/gsoultan/storm/schema/oracle"
)

// OracleDriverPath is the database/sql driver storm's Oracle support is
// written against, named so a refusal can say which import is missing.
//
// Named rather than taken from a flag: a driver is a wire-protocol
// implementation, and two are not interchangeable at the level storm reads —
// runtime/valdec's mappings were MEASURED against this one. A different driver
// needs its own measurement, not a different string.
const OracleDriverPath = "github.com/sijms/go-ora/v2"

// missingDriver turns database/sql's "unknown driver" into an instruction.
//
// The driver is the ADOPTER'S — a prebuilt storm binary cannot link every one
// — so this is the failure a hand-written tool.Main hits first, and go's own
// message ("forgotten import?") names neither the package nor where to put it.
// `storm`'s generated bootstrap adds the import itself; a hand-written main
// has to be told.
func missingDriver(driver string, err error) error {
	if !strings.Contains(err.Error(), "unknown driver") {
		return err
	}
	return fmt.Errorf("%w\n"+
		"       storm reaches Oracle through database/sql, and the DRIVER is yours to\n"+
		"       choose — a prebuilt storm binary cannot link every one. Add it to the\n"+
		"       file that calls tool.Main:\n\n"+
		"           import _ %q\n\n"+
		"       then: go get %s\n"+
		"       (`storm` run without a hand-written bootstrap adds this itself)",
		err, OracleDriverPath, OracleDriverPath)
}

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
		return nil, missingDriver(driver, err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, missingDriver(driver, err)
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
