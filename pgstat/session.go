package pgstat

// Session is one backend connected to the current database.
type Session struct {
	PID             int32
	ApplicationName string
	// State is pg_stat_activity's: active, idle, idle in transaction, and so
	// on. Empty for a backend that reports none.
	State string
	Query string
	// WaitEventType and WaitEvent say what the session is waiting for, if
	// anything. A lock wait is WaitEventType "Lock".
	WaitEventType string
	WaitEvent     string
	// BlockedBy are the sessions holding a lock this one waits for, or queued
	// ahead of it for one.
	BlockedBy []int32
	// Locks are the relation locks the session holds or waits for.
	Locks []Lock
}

// WaitsBehind reports whether s waits for a lock pid holds or is queued for.
func (s Session) WaitsBehind(pid int32) bool {
	for _, p := range s.BlockedBy {
		if p == pid {
			return true
		}
	}
	return false
}

// LocksOn reports whether s holds or waits for a lock of kind on table in
// schema. A session queued for a row holds the row's tuple lock while it waits
// for the transaction ahead of it, so this, with WaitsBehind, is what says a
// session is queued for a row of that table. An empty kind is any kind.
func (s Session) LocksOn(kind, schema, table string) bool {
	for _, l := range s.Locks {
		if (kind == "" || l.Type == kind) && l.Schema == schema && l.Table == table {
			return true
		}
	}
	return false
}

// WaitsFor reports whether s waits, ungranted, for a lock of kind on table in
// schema. An empty kind is any kind.
func (s Session) WaitsFor(kind, schema, table string) bool {
	for _, l := range s.Locks {
		if !l.Granted && (kind == "" || l.Type == kind) && l.Schema == schema && l.Table == table {
			return true
		}
	}
	return false
}
