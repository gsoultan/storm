package migrate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
	"github.com/gsoultan/storm/schema/mssql"
)

// Normalisation for SQL Server, which is the same idea as normalize.go's and a
// different mechanism.
//
// PostgreSQL gets a scratch SCHEMA and a search_path pointed at it. SQL Server
// has no search_path: an unqualified name always resolves in the login's
// default schema, and changing that is an ALTER USER whose effect OUTLIVES the
// run. So the scratch namespace is a whole DATABASE — the unit that can be
// created and dropped without leaving anything different behind. The raw-query
// checker reached the same conclusion for the same reason; see
// tool/mstool/raw.go.
//
// The price is a second connection. A SQL Server session is bound to its
// database at LOGIN, so nothing already connected can be moved to the new one,
// which is why this takes a dialer rather than a connection.

// Conn is the slice of a driver this package needs for the targets that are
// not PostgreSQL. runtime.Executor satisfies it, so a *msdrv.Conn does and so
// does a sqldrv.Exec over any database/sql handle.
//
// Dialect-neutral on purpose: it started as MSSQLConn, and Oracle arriving
// made the name a lie before the interface changed at all.
type Conn interface {
	Query(ctx context.Context, sql string, args []any) (runtime.Rows, error)
	Exec(ctx context.Context, sql string, args []any) (int64, error)
}

// MSSQLConn is Conn under the name it shipped as.
//
// An ALIAS rather than a second interface: the two are the same type, so an
// implementation of one is an implementation of the other and no caller has to
// be edited. Kept because it is in the public surface.
type MSSQLConn = Conn

// MSSQLDialer opens a connection to one database on the target server.
//
// database is "" for the one the caller's DSN already names, and a name for
// any other — "master" to create the scratch database, then the scratch
// database itself. The returned func closes the connection and is called on
// every exit path, including failure.
type MSSQLDialer func(ctx context.Context, database string) (MSSQLConn, func(), error)

// normalizeMSSQL serialises this process's use of the scratch database, whose
// name is shared by everything in it. See NormalizeMSSQL.
var normalizeMSSQL sync.Mutex

// NormalizeMSSQL renders a schema as DDL, applies it to a scratch database,
// reads it back, and drops the database. The result is the model expressed
// exactly as SQL Server would store it — which is the only form worth diffing,
// because half of what the model says has no direct representation there. An
// enum becomes an nvarchar and a CHECK; a `bool` becomes a `bit`; a default of
// `gen_random_uuid()` becomes `(newid())`, parenthesised by the server.
//
// The scratch database is dropped on every exit path, including failure.
func NormalizeMSSQL(ctx context.Context, dial MSSQLDialer, s *schema.Schema) (_ *schema.Schema, err error) {
	// Per-process, for the reason Normalize's scratch schema is: two storm
	// processes against one server — two test binaries, two CI jobs — would
	// otherwise share a database, and one drops it mid-apply of the other.
	//
	// Per-process and not per-CALL on purpose: a name derived from the pid is
	// self-cleaning, because the next run with that pid drops whatever a
	// crashed one left behind. A unique name per call would leak a database
	// nothing knows to remove.
	//
	// Which leaves the callers inside ONE process, who would share the name.
	// AutoMSSQL serialises them on the migration lock, but ForMSSQL is also
	// reachable directly — two goroutines running `storm diff` against one
	// server — so the mutex closes that rather than leaving it to a comment.
	normalizeMSSQL.Lock()
	defer normalizeMSSQL.Unlock()
	name := fmt.Sprintf("storm_normalize_%d", os.Getpid())

	admin, closeAdmin, err := dial(ctx, "master")
	if err != nil {
		return nil, fmt.Errorf("connect to master to build the scratch database: %w", err)
	}
	defer closeAdmin()

	if err := dropScratchDB(ctx, admin, name); err != nil {
		return nil, err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE ["+name+"]", nil); err != nil {
		return nil, fmt.Errorf("create scratch database %s: %w\n"+
			"       the account needs CREATE DATABASE permission", name, err)
	}
	defer func() {
		if e := dropScratchDB(context.WithoutCancel(ctx), admin, name); e != nil && err == nil {
			err = e
		}
	}()

	c, closeC, err := dial(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("connect to scratch database %s: %w", name, err)
	}
	defer closeC()

	ddl, err := msddl.Create(s)
	if err != nil {
		return nil, err
	}
	for _, stmt := range splitBatches(ddl) {
		if _, err := c.Exec(ctx, stmt, nil); err != nil {
			return nil, fmt.Errorf("apply model DDL to the scratch database: %w\n  statement: %s", err, stmt)
		}
	}
	return schemamssql.Introspect(ctx, c, "dbo")
}

// ForMSSQL computes the plan that takes the live `namespace` of the database
// the dialer's "" connection names to `want`, normalising the model through a
// scratch database first so expressions are compared in the form SQL Server
// actually stores.
//
// namespace is a SQL Server SCHEMA — "dbo" unless the model lives elsewhere —
// not a database. Which database is the dialer's business.
//
// One constraint that comes from msddl rather than from here: the DDL it
// renders is UNQUALIFIED, so it applies to the login's DEFAULT schema. Asking
// for a namespace the login does not default to compares that schema against
// DDL destined for another one, and the plan is then a long list of tables to
// create. If the model lives outside dbo, give the connection a login whose
// default schema is the one you name.
func ForMSSQL(ctx context.Context, dial MSSQLDialer, namespace string, want *schema.Schema) (Plan, error) {
	if namespace == "" {
		namespace = "dbo"
	}
	norm, err := NormalizeMSSQL(ctx, dial, want)
	if err != nil {
		return Plan{}, err
	}
	c, closeC, err := dial(ctx, "")
	if err != nil {
		return Plan{}, err
	}
	defer closeC()

	cur, err := schemamssql.Introspect(ctx, c, namespace)
	if err != nil {
		return Plan{}, fmt.Errorf("introspect %s: %w", namespace, err)
	}
	return DiffFor(cur, norm, MSSQL)
}

// dropScratchDB removes the scratch database if it is there.
//
// SINGLE_USER first: a connection this process opened and closed can still be
// lingering, and DROP DATABASE fails on one. The whole thing is guarded on
// DB_ID so it is also the "clean up a leftover from a crashed run" path.
func dropScratchDB(ctx context.Context, admin MSSQLConn, name string) error {
	_, err := admin.Exec(ctx, "IF DB_ID(N'"+name+"') IS NOT NULL BEGIN "+
		"ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
		"DROP DATABASE ["+name+"]; END", nil)
	if err != nil {
		return fmt.Errorf("drop scratch database %s: %w", name, err)
	}
	return nil
}

// splitBatches cuts a DDL script at the semicolons that end a statement.
//
// Quote-aware, unlike a strings.Split, because an enum's CHECK carries its
// labels as string literals and a label is allowed to contain a semicolon.
// Splitting on every `;` would cut that constraint in half and apply the
// first piece — a scratch database that silently disagrees with the model is
// worse than one that fails to build.
//
// Bracketed identifiers are skipped for the same reason: `[order;item]` is a
// legal table name.
func splitBatches(ddl string) []string {
	var out []string
	start := 0
	for i := 0; i < len(ddl); i++ {
		switch ddl[i] {
		case '\'':
			for i++; i < len(ddl); i++ {
				if ddl[i] == '\'' {
					// Doubled quote is an escaped one, not the end.
					if i+1 < len(ddl) && ddl[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case '[':
			for i++; i < len(ddl); i++ {
				if ddl[i] == ']' {
					if i+1 < len(ddl) && ddl[i+1] == ']' {
						i++ // an escaped bracket, not the end
						continue
					}
					break
				}
			}
		case ';':
			if s := strings.TrimSpace(ddl[start:i]); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if s := strings.TrimSpace(ddl[start:]); s != "" {
		out = append(out, s)
	}
	return out
}
