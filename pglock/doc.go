// Package pglock takes and lets go of PostgreSQL advisory locks: locks on a
// number the application chooses, not on a row, for work that must have one
// owner across processes.
//
// A session lock (TryLock, Lock) belongs to the session that took it and lasts
// until Unlock on that same session, or until the session ends. A pool lends a
// different session to each call, so run these on a single connection the
// caller holds for the life of the lock — a pgxdrv.Conn over a connection
// acquired from the pool — never on the pool itself. Unlock on a session that
// does not hold the lock answers false and does nothing; on a pool that is a
// release that reports success while the lock stays held.
//
// A transaction lock (TryLockXact) lasts until its transaction ends and has no
// unlock.
//
// One function per statement, like the generated stores. The catalogs and
// functions are PostgreSQL's own; no other back end has them.
package pglock
