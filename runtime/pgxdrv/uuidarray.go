package pgxdrv

import (
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// Fast uuid[] parameter encoding.
//
// `= ANY($1)` is how every relation load batches its children, so the cost of
// encoding the parent-key array is paid on the hot path of the feature storm
// exists for. pgx's generic array codec boxes each element into an `any` and
// builds a per-element encode plan: measured at ~2 allocations per bound id,
// 1,021 for a 500-id load (bench/RESULTS.md).
//
// pgtype.FlatArray does NOT help — it is pgx's own array wrapper and the
// obvious thing to reach for, and it comes out within noise, because the cost
// is inside the codec rather than the wrapper. That negative result is why this
// exists rather than a one-line change.
//
// So: one plan for the whole slice, writing the binary array format straight
// into the output buffer. Everything else — scanning, text format, other Go
// types — delegates to the codec pgx already installed, so this adds a fast
// path without owning a type.

// uuidArrayCodec overrides only the binary encoding of [][16]byte.
type uuidArrayCodec struct{ pgtype.Codec }

func (c uuidArrayCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		if _, ok := value.([][16]byte); ok {
			return uuidArrayPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type uuidArrayPlan struct{}

// Encode writes the Postgres binary array format:
//
//	int32 ndim | int32 hasnull | uint32 elemOID
//	int32 length | int32 lowerBound     (one dimension)
//	int32 len=16 | 16 bytes             (per element)
//
// A storm key array never contains NULL — the ids come from rows that were read
// — so hasnull is zero and no element carries a -1 length.
func (uuidArrayPlan) Encode(value any, buf []byte) ([]byte, error) {
	ids, ok := value.([][16]byte)
	if !ok {
		return nil, fmt.Errorf("pgxdrv: uuid[] plan got %T", value)
	}
	if ids == nil {
		return nil, nil
	}

	// One grow for the whole array: the size is known exactly.
	need := 20 + len(ids)*20
	if cap(buf)-len(buf) < need {
		grown := make([]byte, len(buf), len(buf)+need)
		copy(grown, buf)
		buf = grown
	}

	buf = binary.BigEndian.AppendUint32(buf, 1)                // ndim
	buf = binary.BigEndian.AppendUint32(buf, 0)                // hasnull
	buf = binary.BigEndian.AppendUint32(buf, pgtype.UUIDOID)   // element type
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(ids))) // dimension length
	buf = binary.BigEndian.AppendUint32(buf, 1)                // lower bound
	for i := range ids {
		buf = binary.BigEndian.AppendUint32(buf, 16)
		buf = append(buf, ids[i][:]...)
	}
	return buf, nil
}

// RegisterFastArrays installs storm's fast parameter encoders on a connection's
// type map. Call it from pgxpool.Config.AfterConnect, or use NewPool which
// does.
//
// Exported because a type map belongs to a connection, so an application that
// builds its own pool has to opt in somewhere — and a fast path that only
// appeared when you used storm's constructor would make a benchmark depend on
// which constructor was called.
func RegisterFastArrays(m *pgtype.Map) {
	registerDecimal(m)
	registerInterval(m)
	registerTimeOfDay(m)
	registerScalarArrays(m)
	t, ok := m.TypeForOID(pgtype.UUIDArrayOID)
	if !ok {
		return // a server without uuid[] has nothing to speed up
	}
	m.RegisterType(&pgtype.Type{
		Name:  t.Name,
		OID:   t.OID,
		Codec: uuidArrayCodec{Codec: t.Codec},
	})
}
