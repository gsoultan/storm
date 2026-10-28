package pgstat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// Every column is read as text, so the result is the same whichever format
// the driver asks for.
const (
	backendSQL = "SELECT pg_backend_pid()::text, current_schema()::text"

	sessionsSQL = `SELECT a.pid::text,
       coalesce(a.application_name, ''),
       coalesce(a.state, ''),
       coalesce(a.query, ''),
       coalesce(a.wait_event_type, ''),
       coalesce(a.wait_event, ''),
       array_to_string(pg_blocking_pids(a.pid), ','),
       coalesce((SELECT string_agg(l.locktype || '|' || n.nspname || '|' || c.relname || '|' || l.granted::text, E'\n')
                   FROM pg_locks l
                   JOIN pg_class c ON c.oid = l.relation
                   JOIN pg_namespace n ON n.oid = c.relnamespace
                  WHERE l.pid = a.pid), '')
  FROM pg_stat_activity a
 WHERE a.datname = current_database() AND a.pid <> pg_backend_pid()
 ORDER BY a.pid`

	terminateSQL = "SELECT pg_terminate_backend($1)::text"
)

// Backend is the session ex runs on, and the schema it works in. Call it on a
// transaction or a single connection: a pool answers for whichever connection
// it lends.
func Backend(ctx context.Context, ex runtime.Executor) (pid int32, schema string, err error) {
	rows, err := ex.Query(ctx, backendSQL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("pgstat: read the backend: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, "", errors.Join(errors.New("pgstat: the backend query returned no row"), rows.Err())
	}
	raw := rows.RawValues()
	n, err := strconv.ParseInt(string(raw[0]), 10, 32)
	if err != nil {
		return 0, "", fmt.Errorf("pgstat: read the backend: %w", err)
	}
	return int32(n), string(raw[1]), rows.Err()
}

// Sessions lists every other session connected to the current database.
func Sessions(ctx context.Context, ex runtime.Executor) ([]Session, error) {
	rows, err := ex.Query(ctx, sessionsSQL, nil)
	if err != nil {
		return nil, fmt.Errorf("pgstat: read the sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		raw := rows.RawValues()
		pid, err := strconv.ParseInt(string(raw[0]), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("pgstat: read a session: %w", err)
		}
		s := Session{
			PID:             int32(pid),
			ApplicationName: string(raw[1]),
			State:           string(raw[2]),
			Query:           string(raw[3]),
			WaitEventType:   string(raw[4]),
			WaitEvent:       string(raw[5]),
		}
		if s.BlockedBy, err = pids(string(raw[6])); err != nil {
			return nil, err
		}
		s.Locks = locks(string(raw[7]))
		out = append(out, s)
	}
	return out, rows.Err()
}

// Terminate ends a session, as pg_terminate_backend does. It reports whether
// the server signalled one.
func Terminate(ctx context.Context, ex runtime.Executor, pid int32) (bool, error) {
	rows, err := ex.Query(ctx, terminateSQL, []any{pid})
	if err != nil {
		return false, fmt.Errorf("pgstat: terminate %d: %w", pid, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false, errors.Join(errors.New("pgstat: terminate returned no row"), rows.Err())
	}
	return string(rows.RawValues()[0]) == "true", rows.Err()
}

func pids(list string) ([]int32, error) {
	if list == "" {
		return nil, nil
	}
	parts := strings.Split(list, ",")
	out := make([]int32, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("pgstat: read a blocking session: %w", err)
		}
		out = append(out, int32(n))
	}
	return out, nil
}

func locks(list string) []Lock {
	if list == "" {
		return nil
	}
	lines := strings.Split(list, "\n")
	out := make([]Lock, 0, len(lines))
	for _, line := range lines {
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			continue
		}
		out = append(out, Lock{Type: f[0], Schema: f[1], Table: f[2], Granted: f[3] == "true"})
	}
	return out
}
