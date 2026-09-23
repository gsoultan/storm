package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/schema"
	oraintro "github.com/gsoultan/storm/schema/oracle"
)

// Normalisation for Oracle — the same idea as normalize.go's and a THIRD
// mechanism.
//
// PostgreSQL gets a scratch SCHEMA and a search_path pointed at it. SQL Server
// gets a scratch DATABASE, because it has no search_path and an unqualified
// name always resolves in the login's default schema. Oracle has neither
// problem and a different one: a SCHEMA IS A USER here, so a scratch namespace
// would mean CREATE USER — a server-wide object needing DBA rights that an
// application's account will not have, and one that outlives the run if
// anything goes wrong.
//
// So the scratch namespace is the CONNECTED USER'S OWN, with a name prefix.
// Every object is created, read back and dropped under a name nothing else
// uses, in the schema the caller already has rights to. It is the cheapest of
// the three and the only one that needs no privilege the application lacks.
//
// The cost is that the model's own table names are not the names applied. That
// is fine for normalisation — what comes back is compared by SHAPE, and the
// prefix is stripped before the comparison — and it is the reason this does
// not double as a migration applier.

// prefix is what a scratch object's name is built from. Short, because Oracle's
// identifier limit is 128 and a table called ck_<table>_<column> already eats
// most of it.
const oraScratchPrefix = "sn_"

// normalizeOracle serialises this process's use of the scratch names, for the
// reason NormalizeMSSQL serialises its scratch database: the prefix is
// per-process, so two goroutines would share it.
var normalizeOracle sync.Mutex

// NormalizeOracle renders a schema as DDL, applies it under prefixed names in
// the connected user's own schema, reads it back, and drops it.
//
// The result is the model expressed exactly as Oracle would store it: an enum
// as a VARCHAR2 and a CHECK, a bool as a BOOLEAN, a default parenthesised by
// the server, a width in characters rather than bytes.
//
// Everything is dropped on every exit path, including failure.
func NormalizeOracle(ctx context.Context, c Conn, s *schema.Schema) (_ *schema.Schema, err error) {
	normalizeOracle.Lock()
	defer normalizeOracle.Unlock()

	// Per-process, so two storm processes against one account do not collide.
	prefix := fmt.Sprintf("%s%d_", oraScratchPrefix, os.Getpid())
	scratch, names, err := prefixed(s, prefix)
	if err != nil {
		return nil, err
	}

	stmts, err := oraddl.Statements(scratch)
	if err != nil {
		// The prefix out of the MESSAGE. A refusal from normalisation is about
		// the caller's MODEL, and reporting `sn_3376_mig_orgs.region` names a
		// table nobody wrote — the scratch mechanism leaking into the one
		// place it must not, which is the sentence somebody has to act on.
		return nil, errors.New(strings.ReplaceAll(err.Error(), prefix, ""))
	}
	drop := func() {
		// Detached from the caller's context: the reason this is unwinding may
		// be that the context is gone, and the objects have to go either way.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		for _, n := range names {
			_, _ = c.Exec(ctx, `DROP TABLE `+oraddl.Ident(n)+` CASCADE CONSTRAINTS PURGE`, nil)
		}
	}
	drop() // a leftover from a crashed run with this pid
	defer drop()

	for _, st := range stmts {
		if _, err := c.Exec(ctx, st, nil); err != nil {
			return nil, fmt.Errorf("apply model DDL to the scratch names: %w\n  statement: %s", err, st)
		}
	}

	got, err := oraintro.Introspect(ctx, c, "")
	if err != nil {
		return nil, err
	}
	return unprefixed(got, prefix), nil
}

// prefixed copies a schema with every NAME prefixed, and reports the tables it
// used so they can be dropped.
//
// EVERY name, not just the table's — that was the first draft and it was
// ORA-02264, "name already used by an existing constraint", on the first run.
// A constraint and an index are schema-scoped here, not table-scoped: two
// tables in one schema cannot share an index name, and a scratch table called
// sn_123_mig_orgs still carries a unique called uq_mig_orgs_name, which is the
// live table's. PostgreSQL and SQL Server both scope these to the table, so
// this is the first target where prefixing a table is not enough.
//
// The derived names oraddl builds at RENDER time — an enum's
// ck_<table>_<column> — are prefixed for free, because the table they are
// derived from already is. The ones schema.Normalize materialised BEFORE this
// ran are not, and those are the collisions.
func prefixed(s *schema.Schema, prefix string) (*schema.Schema, []string, error) {
	out := &schema.Schema{Enums: s.Enums}
	names := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		cp := *t
		cp.Name = prefix + t.Name
		if len(cp.Name) > 128 {
			return nil, nil, fmt.Errorf(
				"migrate: table %s is too long to normalise — the scratch prefix %q takes it "+
					"past Oracle's 128-character identifier limit", t.Name, prefix)
		}

		cp.Uniques = make([]*schema.Unique, len(t.Uniques))
		for i, u := range t.Uniques {
			c := *u
			c.Name = prefix + u.Name
			cp.Uniques[i] = &c
		}
		cp.Checks = make([]*schema.Check, len(t.Checks))
		for i, ck := range t.Checks {
			c := *ck
			c.Name = prefix + ck.Name
			cp.Checks[i] = &c
		}
		cp.Indexes = make([]*schema.Index, len(t.Indexes))
		for i, ix := range t.Indexes {
			c := *ix
			c.Name = prefix + ix.Name
			cp.Indexes[i] = &c
		}
		cp.ForeignKeys = make([]*schema.ForeignKey, len(t.ForeignKeys))
		for i, fk := range t.ForeignKeys {
			c := *fk
			c.Name = prefix + fk.Name
			// And the table it points AT, or the DDL refuses.
			c.RefTable = prefix + fk.RefTable
			cp.ForeignKeys[i] = &c
		}

		out.Tables = append(out.Tables, &cp)
		names = append(names, cp.Name)
	}
	return out, names, nil
}

// unprefixed is the inverse, applied to what came back from the catalogue.
//
// The prefix is removed from ANYWHERE in a name rather than only from the
// front, because the two ways a name acquires it put it in different places: a
// declared `uq_mig_orgs_name` becomes `sn_123_uq_mig_orgs_name`, and an enum's
// check derived from the prefixed table becomes `ck_sn_123_mig_orgs_status`.
// One rule covers both.
//
// Tables the prefix does not match are DROPPED rather than kept: the connected
// user's own schema holds the application's real tables, and normalisation must
// return the MODEL's shape and nothing else.
func unprefixed(s *schema.Schema, prefix string) *schema.Schema {
	strip := func(n string) string { return strings.Replace(n, prefix, "", 1) }
	out := &schema.Schema{}
	for _, t := range s.Tables {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}
		cp := *t
		cp.Name = strings.TrimPrefix(t.Name, prefix)
		for _, u := range cp.Uniques {
			u.Name = strip(u.Name)
		}
		for _, ck := range cp.Checks {
			ck.Name = strip(ck.Name)
		}
		for _, ix := range cp.Indexes {
			ix.Name = strip(ix.Name)
			// An index KEY may be an expression naming the table — a partial
			// unique's CASE does not, but a function-based key could.
			for i := range ix.Columns {
				if ix.Columns[i].Expr {
					ix.Columns[i].Name = strip(ix.Columns[i].Name)
				}
			}
		}
		for _, fk := range cp.ForeignKeys {
			fk.Name = strip(fk.Name)
			fk.RefTable = strip(fk.RefTable)
		}
		out.Tables = append(out.Tables, &cp)
	}
	out.Normalize()
	return out
}

// ForOracle computes the plan that takes the live `namespace` to `want`,
// normalising the model first so expressions are compared in the form Oracle
// actually stores.
//
// namespace is an Oracle SCHEMA, which is a USER. Empty means the connected
// one, which is what an application sees and the right default.
func ForOracle(ctx context.Context, c Conn, namespace string, want *schema.Schema) (Plan, error) {
	norm, err := NormalizeOracle(ctx, c, want)
	if err != nil {
		return Plan{}, err
	}
	cur, err := oraintro.Introspect(ctx, c, namespace)
	if err != nil {
		return Plan{}, fmt.Errorf("introspect %s: %w", namespace, err)
	}
	return DiffFor(cur, norm, Oracle)
}
