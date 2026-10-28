package pglock

import (
	"context"
	"errors"
	"fmt"

	"github.com/gsoultan/storm/runtime"
)

// Every answer is read as text, so the result is the same whichever format the
// driver asks for.
const (
	tryLockSQL     = "SELECT pg_try_advisory_lock($1)::text"
	lockSQL        = "SELECT pg_advisory_lock($1)::text"
	unlockSQL      = "SELECT pg_advisory_unlock($1)::text"
	unlockAllSQL   = "SELECT pg_advisory_unlock_all()::text"
	tryLockXactSQL = "SELECT pg_try_advisory_xact_lock($1)::text"
)

// TryLock takes the session lock on key if nobody else holds it, without
// waiting. It reports whether the session now holds it. A session that
// already holds it takes it again, and has to let go of it as many times.
func TryLock(ctx context.Context, ex runtime.Executor, key int64) (bool, error) {
	return answer(ctx, ex, "try to take", tryLockSQL, key)
}

// Lock takes the session lock on key, waiting for whoever holds it for as long
// as ctx allows.
func Lock(ctx context.Context, ex runtime.Executor, key int64) error {
	_, err := read(ctx, ex, "take", lockSQL, []any{key})
	return err
}

// Unlock lets go of the session lock on key once. It reports whether the
// session held it: false means nothing was let go — the lock, if anybody holds
// it, is held on another session.
func Unlock(ctx context.Context, ex runtime.Executor, key int64) (bool, error) {
	return answer(ctx, ex, "let go of", unlockSQL, key)
}

// UnlockAll lets go of every session lock the session holds.
func UnlockAll(ctx context.Context, ex runtime.Executor) error {
	_, err := read(ctx, ex, "let go of every", unlockAllSQL, nil)
	return err
}

// TryLockXact takes the transaction lock on key if nobody else holds it,
// without waiting. Run it in a transaction: the lock lasts until that
// transaction ends.
func TryLockXact(ctx context.Context, ex runtime.Executor, key int64) (bool, error) {
	return answer(ctx, ex, "try to take the transaction", tryLockXactSQL, key)
}

func answer(ctx context.Context, ex runtime.Executor, what, sql string, key int64) (bool, error) {
	raw, err := read(ctx, ex, what, sql, []any{key})
	if err != nil {
		return false, err
	}
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("pglock: %s lock %d: unexpected answer %q", what, key, raw)
}

// read runs one statement that answers one value.
func read(ctx context.Context, ex runtime.Executor, what, sql string, args []any) (string, error) {
	rows, err := ex.Query(ctx, sql, args)
	if err != nil {
		return "", fmt.Errorf("pglock: %s lock: %w", what, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", errors.Join(fmt.Errorf("pglock: %s lock: no answer", what), rows.Err())
	}
	raw := string(rows.RawValues()[0])
	return raw, rows.Err()
}
