// Package mstool is the SQL Server half of the storm CLI.
//
// It is a package of its own for a reason scripts/check/coverage.sh wrote down
// before it was true: every SQL Server addition to tool/ lowered what the
// PostgreSQL CI job could measure while being perfectly well tested in the
// sqlserver job, and the floor had been nudged down twice to follow it. The
// code that needs a SQL Server to exercise it now has its floor beside the
// other three that live in scripts/check/mssql.sh.
package mstool

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdec"
	"github.com/gsoultan/storm/runtime/msdrv"
	"github.com/gsoultan/storm/schema"
)

// Validating storm.SQL against SQL Server.
//
// The escape hatch's safety has one source: every declared statement is checked
// by a REAL SERVER before it is allowed into the generated allow-list. On
// PostgreSQL that is PREPARE, whose descriptor reports the parameter types and
// the result columns in one round trip.
//
// SQL Server has no equivalent of that descriptor on a prepared handle, and it
// has something better for this purpose: two system procedures that answer the
// same two questions about a statement WITHOUT running it.
//
//	sp_describe_undeclared_parameters   what does it take?
//	sp_describe_first_result_set        what does it return?
//
// The order matters and is not obvious: the second refuses a statement whose
// parameters are undeclared, so the first has to run and its answer has to be
// fed in as @params. A client that calls them the other way round gets "Must
// declare the scalar variable @p1" about a variable the caller did declare.

// PrepareRaw is prepareRawQueries for SQL Server.
//
// decls is every declaration to check, passed in rather than read from a
// package global so this can be called with a subset — which is what a test
// does. againstModel selects the scratch database; false checks against the
// connected database as it is.
func PrepareRaw(dsn string, model *schema.Schema, decls []storm.RawDecl,
	againstModel bool) ([]codegen.RawScanner, []string, error) {

	// What a refusal calls the thing it checked against, which is the first
	// question anyone reading one asks.
	scope := "live"
	if againstModel {
		scope = "model"
	}
	if dsn == "" {
		return nil, nil, fmt.Errorf(
			"raw storm.SQL declarations are registered and validating them needs a server: " +
				"pass -dsn sqlserver://user:pass@host:1433?database=... (or set $STORM_DSN)")
	}
	cfg, err := msdrv.ParseDSN(dsn)
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()

	if againstModel {
		// A scratch DATABASE, not a scratch schema.
		//
		// PostgreSQL gets a schema and a search_path; SQL Server has no
		// search_path, so unqualified names always resolve in the user's
		// default schema and a scratch schema would need an ALTER USER that
		// OUTLIVES the run. A database is the unit that can be created and
		// dropped without changing anything about the server afterwards.
		name := fmt.Sprintf("storm_sqlcheck_%d", os.Getpid())
		admin := cfg
		admin.Database = "master"
		ac, err := msdrv.Open(ctx, admin)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to master to build the scratch database: %w", err)
		}
		if _, err := ac.Exec(ctx, "CREATE DATABASE ["+name+"]", nil); err != nil {
			ac.Close()
			return nil, nil, fmt.Errorf(
				"creating a scratch database to check storm.SQL against the MODEL: %w\n"+
					"       the account needs CREATE DATABASE, or check against the live "+
					"schema instead: -raw-schema db", err)
		}
		defer func() {
			// SINGLE_USER first: a connection this process opened and closed
			// can still be lingering, and DROP DATABASE fails on one.
			_, _ = ac.Exec(context.Background(),
				"ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE", nil)
			_, _ = ac.Exec(context.Background(), "DROP DATABASE ["+name+"]", nil)
			ac.Close()
		}()
		cfg.Database = name
	}

	c, err := msdrv.Open(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()

	if againstModel {
		ddl, err := msddl.Create(model)
		if err != nil {
			return nil, nil, err
		}
		for _, stmt := range strings.Split(ddl, ";") {
			if s := strings.TrimSpace(stmt); s != "" {
				if _, err := c.Exec(ctx, s, nil); err != nil {
					return nil, nil, fmt.Errorf("apply model DDL to the scratch database: %w", err)
				}
			}
		}
	}

	var out []codegen.RawScanner
	// Every statement that describes is registered, scanner or not: SQLExec has
	// no row type and still must be pinned, or the exec half of the escape
	// hatch is the hole the query half no longer has.
	var stmts []string
	for _, d := range decls {
		rt, sql := storm.DeclOf(d)
		name := "storm.SQLExec"
		if rt != nil {
			name = "storm.SQL[" + rt.Name() + "]"
		}

		params, err := describeParams(ctx, c, sql)
		if err != nil {
			return nil, nil, fmt.Errorf("%s does not describe against the %s schema:\n  %w",
				name, scope, err)
		}
		// The count a caller must satisfy is scanned off the statement text;
		// the server just reported the real one. An `@p1` inside a string
		// literal is text to the server and a placeholder to the scanner, and
		// that disagreement is a build error here rather than a confusing
		// "wants 2 arguments, got 1" at the first call.
		if want, got := len(params), storm.ArgsOf(d); want != got {
			return nil, nil, fmt.Errorf(
				"%s takes %d parameter(s), but its text scans as %d — an @pn inside a "+
					"string literal reads as a placeholder\n  %s",
				name, want, got, firstLineOf(sql))
		}

		fields, err := describeResult(ctx, c, sql, params)
		if err != nil {
			return nil, nil, fmt.Errorf("%s does not describe against the %s schema:\n  %w",
				name, scope, err)
		}
		stmts = append(stmts, sql)
		if rt == nil {
			if len(fields) > 0 {
				return nil, nil, fmt.Errorf(
					"%s returns %d column(s) — use storm.SQL[T] to read them, or make the "+
						"statement return nothing", name, len(fields))
			}
			continue
		}
		rs, err := codegen.ResolveRawScannerFor(rt, rt.PkgPath(), fields, codegen.DialectMSSQL)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, rs)
	}
	return out, stmts, nil
}

// describeParams asks what a statement takes, and returns the declaration
// sp_describe_first_result_set needs.
func describeParams(ctx context.Context, c *msdrv.Conn, sql string) ([]string, error) {
	rows, err := c.Query(ctx,
		"EXEC sp_describe_undeclared_parameters @tsql = @p1", []any{sql})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := rows.(interface{ Columns() []string }).Columns()
	nameAt := indexOf(cols, "name")
	typeAt := indexOf(cols, "suggested_system_type_name")
	if nameAt < 0 || typeAt < 0 {
		return nil, fmt.Errorf("sp_describe_undeclared_parameters returned no %q column",
			"suggested_system_type_name")
	}
	var sl runtime.Slab
	var out []string
	for rows.Next() {
		v := rows.RawValues()
		out = append(out, msdec.Str(v[nameAt], &sl)+" "+msdec.Str(v[typeAt], &sl))
	}
	return out, rows.Err()
}

// describeResult asks what a statement returns.
//
// @browse_information_mode = 0 keeps the answer to the columns themselves. The
// other modes add key and source-table information, which storm does not read
// and which makes the procedure refuse statements it would otherwise describe.
func describeResult(ctx context.Context, c *msdrv.Conn, sql string,
	params []string) ([]codegen.RawField, error) {
	rows, err := c.Query(ctx,
		"EXEC sp_describe_first_result_set @tsql = @p1, @params = @p2, "+
			"@browse_information_mode = 0",
		[]any{sql, strings.Join(params, ",")})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := rows.(interface{ Columns() []string }).Columns()
	nameAt := indexOf(cols, "name")
	typeAt := indexOf(cols, "system_type_name")
	if nameAt < 0 || typeAt < 0 {
		return nil, fmt.Errorf("sp_describe_first_result_set returned no %q column",
			"system_type_name")
	}
	var sl runtime.Slab
	var out []codegen.RawField
	for rows.Next() {
		v := rows.RawValues()
		out = append(out, codegen.RawField{
			Name:    msdec.Str(v[nameAt], &sl),
			SQLType: msdec.Str(v[typeAt], &sl),
		})
	}
	return out, rows.Err()
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if strings.EqualFold(s, want) {
			return i
		}
	}
	return -1
}

// firstLineOf is how a refusal names the statement it is about: enough to
// find the declaration, not the whole body. A copy of tool's rather than an
// export of it — it is four lines, and an exported helper crossing a package
// boundary for four lines is a worse trade than the copy.
func firstLineOf(sql string) string {
	s := strings.TrimSpace(sql)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " ..."
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
