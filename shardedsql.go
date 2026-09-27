package storm

// The escape hatch, on a sharded table.
//
// `storm.SQL` takes a runtime.Executor, and a shard.Bound satisfies one — so
// routing a raw statement already WORKED. What did not work is the guarantee:
// a pool satisfies runtime.Executor too, so the one mistake sharding exists to
// prevent was expressible again the moment anybody dropped to raw SQL.
//
// That mattered more here than anywhere else. Every other query against a
// sharded model is checked by its parameter type; the escape hatch was the
// single door out of that, and a door out of a compile-time guarantee is the
// guarantee's real strength.

import (
	"context"
	"reflect"

	"github.com/gsoultan/storm/runtime/shard"
)

// ShardedSQLQuery is a raw query against a sharded table. Its Query takes a
// shard.Bound rather than a runtime.Executor, so the statement cannot run on
// a pool.
//
// Declared the same way and validated the same way — `storm generate` PREPAREs
// it, matches the result descriptor against T's fields, and emits a scanner —
// and pinned the same way: only a statement generate saw runs. The single
// difference is the type of the thing it will accept at the call.
type ShardedSQLQuery[T any] struct {
	q SQLQuery[T]
}

// ShardedSQL declares a raw query, against a sharded table, returning rows of T.
//
//	var TenantAging = storm.ShardedSQL[AgingRow](`
//	    SELECT bucket, sum(total) FROM invoices
//	    WHERE tenant_id = $1 GROUP BY bucket`)
//
//	ex, err := shards.For(shard.UUIDKey(tenantID))
//	rows, err := TenantAging.Query(ctx, ex, tenantID)
//
// The statement still has to filter by the shard key itself. storm routes the
// statement to the right database; it does not read your WHERE clause, and a
// raw query that omits the tenant predicate returns that shard's other
// tenants. That is the part the escape hatch cannot check for you, and it is
// why the typed query API is the better answer wherever it reaches.
func ShardedSQL[T any](sql string) *ShardedSQLQuery[T] {
	return &ShardedSQLQuery[T]{q: SQLQuery[T]{sql: sql, digest: digestOf(sql), nArg: maxPlaceholder(sql)}}
}

// Query runs the statement on one shard and scans every row.
func (q *ShardedSQLQuery[T]) Query(ctx context.Context, ex shard.Bound, args ...any) ([]T, error) {
	return q.q.Query(ctx, ex, args...)
}

// One runs the statement on one shard and returns the first row, if any.
func (q *ShardedSQLQuery[T]) One(ctx context.Context, ex shard.Bound, args ...any) (T, bool, error) {
	return q.q.One(ctx, ex, args...)
}

// SQL is the statement's text, for a caller assembling a migration or a log
// line. It is the same text generate PREPAREd.
func (q *ShardedSQLQuery[T]) SQL() string { return q.q.sql }

func (q *ShardedSQLQuery[T]) decl() (reflect.Type, string, int, bool) {
	rt, s, n, _ := q.q.decl()
	return rt, s, n, true
}

// ShardedOf reports whether a declaration was declared against a sharded
// table. The generate command uses it to refuse a plain storm.SQL whose text
// names a sharded table, and nothing else should.
//
// A method on RawDecl rather than a type assertion for the reason ADR-0005
// gives: a capability discovered at run time is a capability that can be
// silently absent. Every declaration answers this question, and the two that
// answer "no" are the two that cannot carry a shard.
func ShardedOf(d RawDecl) bool { _, _, _, sh := d.decl(); return sh }
