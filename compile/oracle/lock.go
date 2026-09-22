package oracle

// Row locking on Oracle, which refuses two combinations nothing else storm
// targets refuses.
//
// 1. THERE IS NO SHARED ROW LOCK. `FOR UPDATE` has no `FOR SHARE` counterpart.
//    `LOCK TABLE … IN SHARE MODE` exists and is a different promise entirely —
//    it locks the TABLE, so it blocks every writer rather than the rows this
//    query read. Substituting it would turn a narrow lock into a global one
//    without the caller asking, which is the failure this package exists to
//    prevent. The three share modes are refused by name.
//
// 2. A ROW CAP AND A ROW LOCK CANNOT BE COMBINED. `… FETCH FIRST n ROWS ONLY
//    FOR UPDATE SKIP LOCKED` is ORA-02014: Oracle implements FETCH FIRST as an
//    inline view with a window function, and it will not lock through one.
//
//    This is the WORK QUEUE — claim the oldest n pending rows, skipping what
//    another worker holds — and it is one statement on PostgreSQL, MySQL and
//    SQL Server. `WHERE ROWNUM <= n … FOR UPDATE SKIP LOCKED` parses, and it is
//    NOT the same statement: ROWNUM is assigned before ORDER BY, so it takes n
//    arbitrary rows rather than the n oldest. Emitting that would answer a
//    different question with the same API, which is worse than refusing.
//
//    Measured before this package was written; see internal/oraclespike,
//    result 1.

// Lock modes, matching the seam's numbering.
const (
	LockNone = iota
	LockUpdate
	LockUpdateNoWait
	LockUpdateSkipLocked
	LockShare
	LockShareNoWait
	LockShareSkipLocked
	numLockModes
)

// NumLockModes is how many lock states a generated Query can be in.
const NumLockModes = int(numLockModes)

// LockSuffix is the trailing clause. Unlike SQL Server, where the lock is a
// table hint and this is empty for every mode, Oracle's is a suffix like
// PostgreSQL's — for the three modes it has.
func LockSuffix(m int) string {
	switch m {
	case LockUpdate:
		return " FOR UPDATE"
	case LockUpdateNoWait:
		return " FOR UPDATE NOWAIT"
	case LockUpdateSkipLocked:
		return " FOR UPDATE SKIP LOCKED"
	}
	return ""
}

// LockHint is empty for every mode: the lock is a suffix here, not a hint. It
// exists because the seam has both shapes and SQL Server needed the other one.
func LockHint(int) string { return "" }

// LockRefused is why a mode has no Oracle form, or "".
//
// Returned rather than approximated. A generated package that offered
// ForShare() and gave a table lock would be a package whose method name is a
// lie, and the caller would find out under load.
func LockRefused(m int) string {
	switch m {
	case LockShare, LockShareNoWait, LockShareSkipLocked:
		return "Oracle has no shared ROW lock: FOR UPDATE has no FOR SHARE counterpart, " +
			"and LOCK TABLE ... IN SHARE MODE locks the whole table rather than the rows " +
			"this query read"
	}
	return ""
}

// LockRefusedCapped is why a locked read may not also be capped.
//
// The one refusal in this package that is about a COMBINATION rather than a
// construct, and the one that will cost an adopter the most: every work queue
// is written this way.
const LockRefusedCapped = "Oracle cannot lock a capped read: FETCH FIRST is an inline view " +
	"and locking through one is ORA-02014. ROWNUM parses but is assigned BEFORE ORDER BY, " +
	"so it would claim n arbitrary rows rather than the n oldest — a different question " +
	"with the same API"

// LockNotes is what a generated package's doc comment says about locking here.
//
// It lives in this package rather than in codegen for the reason R9 exists: the
// text names KEYWORDS, and a keyword in codegen is a keyword one dialect owns
// leaking into every other one's output.
func LockNotes() []string {
	return []string{
		"A lock is held to the end of the TRANSACTION, so one taken outside a",
		"transaction is released before the next statement runs and protects",
		"nothing — pass a transaction, not a pool.",
		"",
		"This target has THREE lock modes, not six. Oracle has no shared row",
		"lock: FOR UPDATE has no FOR SHARE counterpart, and LOCK TABLE ... IN",
		"SHARE MODE locks the table rather than the rows this query read. The",
		"shared modes are refused rather than widened.",
		"",
		"And a locked read may not be CAPPED. FETCH FIRST is an inline view",
		"here and locking through one is ORA-02014, so the work-queue shape",
		"— claim the oldest n rows, skipping what another worker holds — is",
		"one statement on every other target and is refused on this one.",
		"ROWNUM parses but is assigned before ORDER BY, which would claim n",
		"arbitrary rows rather than the n oldest.",
		"",
		"Locking is refused on Count and Exists, and on the declared",
		"aggregations and joins, for the reason it is refused everywhere else:",
		"a lock combined with an aggregate or a set operation locks something",
		"other than the rows the caller is looking at.",
	}
}
