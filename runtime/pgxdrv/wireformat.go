package pgxdrv

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Generated scanners decode the RAW wire bytes, which means raorm has a wire
// FORMAT requirement that nothing used to state or check.
//
// Postgres can send every column as binary or as text, and pgx chooses per
// type. For most types the two encodings are nothing alike: `false` is one
// zero byte in binary and the byte 'f' in text, so `runtime.Bool` — which
// asks whether the first byte is non-zero — reports TRUE for a text `false`.
// An int8 is eight big-endian bytes in binary and ASCII digits in text, so
// the decoder panics on a short slice. Neither is a failure a caller can see
// coming, and the first is silent.
//
// The way an adopter arrives here is ordinary: pgx's simple-protocol and exec
// modes send everything as text, and both are the documented settings for
// running behind PgBouncer in transaction pooling mode. One pool option, and
// every boolean in the system inverts.
//
// So the format is checked, once per result, against what each column's type
// actually needs.

// binaryFormat is the pgx/pgconn format code for binary; text is 0.
const binaryFormat int16 = 1

// formatOK reports whether raorm can decode this column in the format the
// server chose.
//
// Binary is always fine. The interesting question is which types are BROKEN
// by text, and the answer is an exact, closed list: the built-in types raorm
// has a binary-only decoder for. Naming those rather than naming the safe
// ones is deliberate, because the safe set is open-ended and a check that
// rejects what it does not recognise rejects working schemas:
//
//   - text, varchar, name are the same bytes either way, which is why pgx
//     does not request binary for them (measured: format 0 on a binary
//     connection).
//   - jsonb's binary form is the same document behind one version byte, which
//     runtime.JSONB already strips; pgx sends it as text by default.
//   - **enums and other user-defined types** send their label as text, and
//     that label IS the value raorm scans into a string. The fixture's
//     `status` enum is exactly this, and an "everything must be binary" rule
//     failed four tests on it before this was written that way round.
//   - domains are transparent on the wire: PostgreSQL reports the BASE type's
//     OID in the row description, so a domain over int8 is checked as int8
//     and gets no free pass.
//
// Everything on the list below decodes from a fixed binary layout, so a text
// value is corruption — silently for bool, loudly (a short-slice panic) for
// the fixed-width numbers.
func formatOK(oid uint32, format int16) bool {
	if format == binaryFormat {
		return true
	}
	switch oid {
	case 16, // bool — 'f' is 0x66, non-zero, so text false reads as TRUE
		21, 23, 20, // int2, int4, int8
		700, 701, // float4, float8
		17,               // bytea — text is the \x hex escape, not the bytes
		2950,             // uuid — text is 36 dashed characters, not 16 bytes
		1184, 1082, 1186, // timestamptz, date, interval
		1700,     // numeric
		869, 650, // inet, cidr
		1009, 1015, // text[], varchar[]
		2951, // uuid[]
		1016: // int8[]
		return false
	}
	return true
}

// checkFormats fails a result whose columns raorm cannot decode, naming the
// first offending column and the fix.
//
// It runs once per result, not once per row: field descriptors arrive with
// the row description, before the first Next, so a thousand-row scan pays for
// one loop over its columns.
func checkFormats(r pgx.Rows) error {
	for i, fd := range r.FieldDescriptions() {
		if formatOK(fd.DataTypeOID, fd.Format) {
			continue
		}
		return fmt.Errorf(
			"raorm: column %d %q (oid %d) arrived in text format and raorm decodes binary — "+
				"this connection is in QueryExecModeSimpleProtocol or QueryExecModeExec, "+
				"often set for PgBouncer transaction pooling. Use pgx's default "+
				"QueryExecModeCacheStatement (or CacheDescribe/DescribeExec); see docs/DEPLOYMENT.md",
			i+1, fd.Name, fd.DataTypeOID)
	}
	return nil
}

// newRows wraps a pgx result after checking it can be decoded. On refusal the
// result is closed here: the caller gets an error and no Rows, so there is
// nothing left for them to close.
func newRows(r pgx.Rows) (rows, error) {
	if err := checkFormats(r); err != nil {
		r.Close()
		return rows{}, err
	}
	return rows{r}, nil
}

// refuseTextModes rejects a pool config that would send everything as text.
//
// The per-result check above is the real guarantee, since a caller can build
// their own pool and wrap it. This exists so the common case fails at
// construction — once, with the fix in hand — instead of on the first query
// of the first request.
func refuseTextModes(mode pgx.QueryExecMode) error {
	switch mode {
	case pgx.QueryExecModeSimpleProtocol, pgx.QueryExecModeExec:
		return fmt.Errorf(
			"raorm: this pool is configured with %v, which sends every value as text; "+
				"raorm's generated scanners decode binary, so booleans would invert silently. "+
				"If this was set for PgBouncer, use transaction pooling with pgx's default "+
				"QueryExecModeCacheStatement instead; see docs/DEPLOYMENT.md", mode)
	}
	return nil
}
