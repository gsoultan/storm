package migrate

import (
	"context"
	"fmt"
	"os"

	"github.com/gsoultan/raorm/compile/pgddl"
	"github.com/gsoultan/raorm/schema"
	pgintro "github.com/gsoultan/raorm/schema/pg"
	"github.com/jackc/pgx/v5"
)

// PostgreSQL rewrites every expression it stores. The model says
//
//	CHECK (age IS NULL OR age BETWEEN 0 AND 150)
//	DEFAULT 'pending'
//	UNIQUE (lower(email))
//
// and the catalog returns
//
//	CHECK ((age IS NULL) OR ((age >= 0) AND (age <= 150)))
//	DEFAULT 'pending'::status
//	UNIQUE (lower((email)::text))
//
// No amount of string canonicalisation makes those compare equal in general —
// BETWEEN genuinely becomes two comparisons. So raorm does not try. It runs the
// model through PostgreSQL first and diffs catalog form against catalog form.
//
// This is why ADR-0001 assumes a dev database. It is the same one that
// `raorm.SQL[T]` needs for PREPARE validation, and an offline snapshot works
// equally well.

// Executor is the slice of pgx that normalisation needs.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconnTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type pgconnTag = interface{ String() string }

// Normalize renders a schema as DDL, applies it to a scratch namespace, reads
// it back, and drops the namespace. The result is the model expressed exactly
// as PostgreSQL would store it.
//
// The scratch namespace is dropped on every exit path, including failure.
func Normalize(ctx context.Context, c *pgx.Conn, scratch string, s *schema.Schema) (_ *schema.Schema, err error) {
	if scratch == "" {
		// Per-process, not a shared literal: two processes normalising against
		// one database — two test binaries under `go test ./...`, or two CI
		// jobs — otherwise share a schema, and one drops it mid-apply of the
		// other ("referenced schema was concurrently dropped"). Within a
		// process the callers are sequential, so the pid is enough.
		scratch = fmt.Sprintf("raorm_normalize_%d", os.Getpid())
	}
	if err := validIdent(scratch); err != nil {
		return nil, err
	}

	// Preserve the caller's search_path: normalisation must not be observable.
	var prev string
	if err := c.QueryRow(ctx, "SHOW search_path").Scan(&prev); err != nil {
		return nil, fmt.Errorf("read search_path: %w", err)
	}
	defer func() {
		_, _ = c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE")
		if _, e := c.Exec(ctx, "SET search_path TO "+prev); e != nil && err == nil {
			err = fmt.Errorf("restore search_path: %w", e)
		}
	}()

	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+scratch+" CASCADE; CREATE SCHEMA "+scratch); err != nil {
		return nil, fmt.Errorf("create scratch schema: %w", err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+scratch); err != nil {
		return nil, err
	}
	if _, err := c.Exec(ctx, pgddl.Create(s)); err != nil {
		return nil, fmt.Errorf("apply model DDL to scratch schema: %w", err)
	}
	return pgintro.Introspect(ctx, c, scratch)
}

// For computes the plan that takes the live `target` namespace to `want`,
// normalising the model through a scratch namespace first so expressions are
// compared in the form PostgreSQL actually stores.
func For(ctx context.Context, c *pgx.Conn, target string, want *schema.Schema) (Plan, error) {
	norm, err := Normalize(ctx, c, "", want)
	if err != nil {
		return Plan{}, err
	}
	cur, err := pgintro.Introspect(ctx, c, target)
	if err != nil {
		return Plan{}, fmt.Errorf("introspect %s: %w", target, err)
	}
	plan := Diff(cur, norm)
	// A plan that creates an exclusion constraint needs btree_gist FIRST. On a
	// long-lived dev database something installed the extension years ago; on
	// a fresh production database the migration is the first thing that ever
	// ran, and without this line it fails there and nowhere else. IF NOT
	// EXISTS makes it free where the extension already lives.
	if !plan.Empty() && pgddl.NeedsBtreeGist(want) {
		plan.Changes = append([]Change{{SQL: pgddl.BtreeGistDDL}}, plan.Changes...)
	}
	return plan, nil
}

// validIdent guards the one place a name is interpolated into SQL. Scratch
// namespaces come from configuration, never from a request, but the check costs
// nothing and the alternative is an injection point in a migration tool.
func validIdent(s string) error {
	if s == "" || len(s) > 63 {
		return fmt.Errorf("invalid schema name %q", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c == '_' || (i > 0 && (c >= '0' && c <= '9'))
		if !ok {
			return fmt.Errorf("invalid schema name %q: only lowercase letters, digits and underscore", s)
		}
	}
	return nil
}
