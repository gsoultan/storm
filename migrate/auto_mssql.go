package migrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/schema"
)

// Automigrate for SQL Server.
//
// Same promises as Auto's, listed at the top of auto.go, and three of them are
// kept by a DIFFERENT mechanism because this engine disagrees with PostgreSQL
// about what a session is:
//
//	the lock          sp_getapplock, not pg_advisory_lock
//	the step bound    SET LOCK_TIMEOUT, not lock_timeout
//	the namespace     nothing. There is no search_path to point at one.
//
// And one promise is kept MORE easily. PostgreSQL's plan splits three ways —
// enum label additions first and alone, then the transaction, then the steps
// that cannot be in one — and every split is a place a failure can leave the
// schema half moved. Neither split exists here: SQL Server has no enum type to
// add a label to, and no non-blocking index build to run outside a transaction
// (see the seam's CreateIndexConcurrently comment). So a SQL Server migration
// is ONE transaction, always, and its DDL is transactional, so a failure
// anywhere leaves the schema exactly where it began.
//
// The session is storm's own. AutoMSSQL takes a dialer rather than a
// connection, because normalisation needs a second database and a session here
// is bound to its database at LOGIN — and the consequence is worth naming: the
// session that holds the lock is opened and closed by this call, so there is
// nothing to restore on the way out and no lock that can outlive a failure.
// Auto has to be careful about both, because AutoPool lends it a connection
// that goes back to serving application queries afterwards.

// AutoMSSQL brings the dialer's database up to date with `want`, and reports
// what it applied.
//
// The DDL msddl renders is UNQUALIFIED, so it lands in the login's DEFAULT
// schema. AutoMSSQL refuses to start when that is not the schema it was asked
// to migrate, rather than writing tables into one schema and re-diffing another
// forever. If the model lives outside dbo, give the connection a login whose
// default schema is the one you name.
func AutoMSSQL(ctx context.Context, dial MSSQLDialer, want *schema.Schema, o AutoOptions) (Result, error) {
	ns := o.mssqlNamespace()
	if err := validMSSQLIdent(ns); err != nil {
		return Result{}, err
	}
	if o.Concurrently {
		return Result{}, errors.New("migrate: AutoOptions.Concurrently has no SQL Server form: " +
			"the non-blocking index build there is WITH (ONLINE = ON), an Enterprise edition " +
			"feature, so storm does not emit it")
	}
	// What the model says for another dialect's sake cannot be expressed here,
	// and finding that out from a failed ALTER on a live database is the wrong
	// end of the process. Check reports every problem at once, which is the
	// difference between one deploy and five.
	if err := msddl.Check(want); err != nil {
		return Result{}, err
	}

	c, closeC, err := dial(ctx, "")
	if err != nil {
		return Result{}, err
	}
	defer closeC()

	if err := requireDefaultSchema(ctx, c, ns); err != nil {
		return Result{}, err
	}

	waited, err := lockMSSQL(ctx, c, ns, o)
	if err != nil {
		return Result{Waited: waited}, err
	}
	res := Result{Waited: waited}
	defer func() {
		// The connection closing is what actually guarantees this — SQL Server
		// drops a session-owned applock when the session ends, and closeC runs
		// after this. The explicit release is for the log line it saves the
		// next caller, not for correctness.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if _, err := c.Exec(ctx, "EXEC sp_releaseapplock @Resource = N"+
			quote(applockName(ns))+", @LockOwner = 'Session'", nil); err != nil {
			o.logf("storm: releasing the migration lock failed (it is released on disconnect): %v", err)
		}
	}()

	// Nothing to create first. Auto creates the namespace at this point because
	// introspecting a schema that does not exist reports an empty one and the
	// plan would then CREATE TABLE into nowhere. Here the namespace is the
	// login's default schema, which exists by definition — a login cannot
	// default to a schema that is not there — and requireDefaultSchema above
	// has already established that it is the one being migrated.

	// AFTER the lock. A replica that waited its turn must diff against what
	// the winner just applied, or it applies the same plan a second time.
	plan, err := ForMSSQL(ctx, dial, ns, want)
	if err != nil {
		return res, err
	}
	if plan.Empty() {
		o.logf("storm: schema %s already matches the model", ns)
		return res, nil
	}
	if plan.Destructive() && !o.AllowDestructive {
		return res, &DestructiveError{Plan: plan}
	}
	for _, ch := range plan.Changes {
		if ch.NoTransaction {
			// Unreachable today and checked anyway: the only thing that sets
			// this is Plan.Concurrently, which is the identity here. A step
			// that quietly joined the transaction would be a step SQL Server
			// refuses, discovered during a deployment.
			return res, fmt.Errorf("migrate: a SQL Server plan contains a step marked "+
				"NoTransaction, and this engine has no statement that needs one:\n  %s", ch.SQL)
		}
	}

	applied, err := applyTxMSSQL(ctx, c, plan.Changes, o)
	res.Applied = append(res.Applied, applied...)
	if err != nil {
		return res, err
	}
	o.logf("storm: applied %d step(s) to schema %s", len(res.Applied), ns)
	return res, nil
}

// mssqlNamespace is namespace with SQL Server's default rather than
// PostgreSQL's. "public" is not a SQL Server convention at all — there is no
// such schema — so the zero value has to mean something different here, and a
// caller who names one gets the one they named.
func (o AutoOptions) mssqlNamespace() string {
	if o.Schema == "" {
		return "dbo"
	}
	return o.Schema
}

// applyTxMSSQL runs every step in one transaction.
//
// SET XACT_ABORT ON is what makes that true rather than nearly true. Without
// it, a SQL Server run-time error inside a transaction aborts the STATEMENT and
// leaves the transaction open and the earlier steps live — so a migration that
// failed half way would commit its first half on the next COMMIT, or worse, sit
// there holding schema locks. With it, any error rolls the whole thing back.
func applyTxMSSQL(ctx context.Context, c MSSQLConn, steps []Change, o AutoOptions) ([]Change, error) {
	if err := boundMSSQL(ctx, c, o); err != nil {
		return nil, err
	}
	if _, err := c.Exec(ctx, "SET XACT_ABORT ON; BEGIN TRANSACTION", nil); err != nil {
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// @@TRANCOUNT because XACT_ABORT may have rolled it back already, and
		// a ROLLBACK with no transaction open is itself an error — one that
		// would replace the real failure in the log.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = c.Exec(ctx, "IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION", nil)
	}()

	for _, ch := range steps {
		if _, err := c.Exec(ctx, ch.SQL, nil); err != nil {
			return nil, fmt.Errorf("%w\n  while applying: %s\n  nothing was applied: the transaction rolled back", err, ch.SQL)
		}
		o.logf("storm: %s", ch.SQL)
	}
	if _, err := c.Exec(ctx, "COMMIT TRANSACTION", nil); err != nil {
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	committed = true
	return steps, nil
}

// boundMSSQL bounds how long any one step waits for a lock it needs.
//
// On the SESSION, because SQL Server has no transaction-scoped form of this —
// there is no SET LOCAL. That costs nothing here that it would cost in Auto:
// the session is storm's own and is closed at the end of the call, so a setting
// left behind has nobody to surprise.
func boundMSSQL(ctx context.Context, c MSSQLConn, o AutoOptions) error {
	ms := int64(-1) // SQL Server's own default: wait forever
	if d := o.lockTimeout(); d > 0 {
		if ms = d.Milliseconds(); ms == 0 {
			ms = 1 // a sub-millisecond bound is not "no bound"
		}
	}
	if _, err := c.Exec(ctx, "SET LOCK_TIMEOUT "+strconv.FormatInt(ms, 10), nil); err != nil {
		return fmt.Errorf("set LOCK_TIMEOUT: %w", err)
	}
	return nil
}

// requireDefaultSchema refuses to migrate a schema the unqualified DDL will not
// land in.
//
// This is the guard for the one thing SQL Server cannot do that PostgreSQL can.
// Auto points search_path at its namespace and every unqualified name follows.
// There is no such setting here: a name with no schema resolves in the LOGIN's
// default schema, and changing that is an ALTER USER whose effect outlives the
// process. So the only honest options are to migrate that schema or to refuse,
// and refusing beats the alternative — writing every table into one schema
// while diffing another, which produces a plan that never empties and a
// migration that runs again on every start.
func requireDefaultSchema(ctx context.Context, c MSSQLConn, ns string) error {
	_, err := c.Exec(ctx, "DECLARE @storm_ns sysname = SCHEMA_NAME();\n"+
		"IF @storm_ns <> N"+quote(ns)+" RAISERROR("+
		"'storm: unqualified DDL lands in schema %s, not "+ns+": the DDL storm renders for "+
		"SQL Server names no schema, so it follows the login''s default. "+
		"Use a login whose default schema is "+ns+", or migrate %s instead.', 16, 1, "+
		"@storm_ns, @storm_ns);", nil)
	return err
}

// lockMSSQL serialises migrating processes on a session-owned application lock,
// and reports how long it waited.
//
// @LockTimeout = 0 is the try-and-fail-now form, polled, for the reason Auto
// polls pg_try_advisory_lock: the blocking form's own wait is the only bound
// available to it, and a caller who wants LockWait to mean something needs the
// loop. @LockOwner = 'Session' is what makes the lock outlive the statement
// without needing a transaction open around it.
//
// RAISERROR rather than reading the procedure's return value, because that
// return arrives as a scalar this package would have to decode from the wire —
// and an error is what the caller does with it either way.
func lockMSSQL(ctx context.Context, c MSSQLConn, ns string, o AutoOptions) (time.Duration, error) {
	stmt := "DECLARE @storm_lock int;\n" +
		"EXEC @storm_lock = sp_getapplock @Resource = N" + quote(applockName(ns)) +
		", @LockMode = 'Exclusive', @LockOwner = 'Session', @LockTimeout = 0;\n" +
		"IF @storm_lock < 0 RAISERROR('" + applockBusy + " (%d)', 16, 1, @storm_lock);"

	start := time.Now()
	deadline := start.Add(o.lockWait())
	for attempt := 0; ; attempt++ {
		_, err := c.Exec(ctx, stmt, nil)
		if err == nil {
			if attempt > 0 {
				o.logf("storm: took the migration lock after %s", time.Since(start).Round(time.Millisecond))
			}
			return time.Since(start), nil
		}
		if !strings.Contains(err.Error(), applockBusy) {
			// Not "somebody else has it" — no permission, no connection, a
			// server that has never heard of sp_getapplock. Say so rather than
			// spend LockWait pretending to queue.
			return time.Since(start), fmt.Errorf("take the migration lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			return time.Since(start), fmt.Errorf("%w: waited %s for schema %s",
				ErrLockBusy, time.Since(start).Round(time.Millisecond), ns)
		}
		if attempt == 0 {
			o.logf("storm: another process is migrating schema %s; waiting up to %s", ns, o.lockWait())
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// applockBusy is the sentinel lockMSSQL raises and then looks for, to tell
// "another process holds it" from "this did not work".
const applockBusy = "storm: migration lock busy"

// applockName is the resource every storm process migrating this namespace
// agrees on. A STRING rather than Auto's hashed int64, because sp_getapplock
// takes one — and the same rule applies: it must be stable across builds and
// versions, since two replicas on different storm versions during a rolling
// deploy are exactly the case the lock exists for.
func applockName(ns string) string { return "storm:automigrate:" + ns }

// validMSSQLIdent guards the names this package pastes into SQL.
//
// Separate from validIdent, which is PostgreSQL's: that one refuses uppercase,
// and `dbo` is the only SQL Server schema name that would survive it by
// accident. The limit is 128 because sysname is nvarchar(128).
func validMSSQLIdent(s string) error {
	if s == "" || len(s) > 128 {
		return fmt.Errorf("invalid schema name %q", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("invalid schema name %q: only letters, digits and underscore", s)
		}
	}
	return nil
}
