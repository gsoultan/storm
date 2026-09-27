package storm

import (
	"context"
	"reflect"

	"github.com/gsoultan/storm/runtime/shard"
)

// ShardedSQLStmt is the no-rows half, against a sharded table.
type ShardedSQLStmt struct {
	s SQLStmt
}

// ShardedSQLExec declares a raw statement, against a sharded table, executed
// for its effect.
func ShardedSQLExec(sql string) *ShardedSQLStmt {
	return &ShardedSQLStmt{s: SQLStmt{sql: sql, digest: digestOf(sql), nArg: maxPlaceholder(sql)}}
}

// Exec runs the statement on one shard and reports rows affected.
func (q *ShardedSQLStmt) Exec(ctx context.Context, ex shard.Bound, args ...any) (int64, error) {
	return q.s.Exec(ctx, ex, args...)
}

// SQL is the statement's text.
func (q *ShardedSQLStmt) SQL() string { return q.s.sql }

func (q *ShardedSQLStmt) decl() (reflect.Type, string, int, bool) {
	rt, s, n, _ := q.s.decl()
	return rt, s, n, true
}

var (
	_ RawDecl = (*ShardedSQLQuery[struct{}])(nil)
	_ RawDecl = (*ShardedSQLStmt)(nil)
)
