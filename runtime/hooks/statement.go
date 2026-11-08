package hooks

import (
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Kind is which of the Executor's four methods a Statement went through.
type Kind uint8

const (
	KindQuery Kind = iota + 1 // Executor.Query: rows come back
	KindExec                  // Executor.Exec: a count comes back
	KindCopy                  // Executor.CopyFrom: a bulk load
	KindBatch                 // Executor.Batch: several statements, one round trip
)

var kindNames = [...]string{"unknown", "query", "exec", "copy", "batch"}

// String is the lower-case method name, suitable for a span or metric name.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return kindNames[0]
}

// Statement is one call through a wrapped Executor. Before sees the fields
// that describe the call; After sees those plus the outcome.
//
// The Statement is the hook's for the duration of the call only: the Args,
// Columns and Ops slices belong to the caller and may be reused once the
// statement finishes, so copy anything kept past After.
type Statement struct {
	Kind Kind

	// SQL and Args are set for KindQuery and KindExec.
	SQL  string
	Args []any

	// Table and Columns are set for KindCopy.
	Table   string
	Columns []string

	// Ops is set for KindBatch.
	Ops []runtime.BatchOp

	// Start is when the statement was handed to the driver.
	Start time.Time

	// Duration, Rows and Err are the outcome, set before After runs. Rows is
	// the number of rows read for KindQuery, the count the database reported
	// for KindExec and KindCopy, and zero for KindBatch.
	Duration time.Duration
	Rows     int64
	Err      error
}
