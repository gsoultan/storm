package pgstat

// Lock is a lock a session holds or waits for on a relation.
type Lock struct {
	// Type is pg_locks.locktype: relation, tuple, and so on.
	Type    string
	Schema  string
	Table   string
	Granted bool
}
