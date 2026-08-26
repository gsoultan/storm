package pgxdrv

import (
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// Fast int8[] and text[] parameter encoding, for the same reason uuid[] has
// one: `= ANY($1)` is how every relation load batches its children, so the
// parent-key array is encoded on the hot path of the feature raorm exists for.
//
// uuid[] was built first because raorm's own fixtures use uuid keys. A schema
// with bigserial primary keys — which is most Postgres schemas that are not
// uuid-first — binds int8[] on exactly the same path, and a natural key binds
// text[]. Measured before writing this, 500 elements through pgx's generic
// codec (bench/RESULTS.md):
//
//	int8[]  466 allocations, 6,268 ns
//	text[]  503 allocations, 9,118 ns
//
// About one allocation per element in both, which is the same disease uuid[]
// had at 1,003. The cure is the same: one plan for the whole slice, writing
// the binary array format straight into the output buffer, delegating
// everything else — scanning, text format, other Go types — to the codec pgx
// already installed.

// arrayHeaderInto writes the five fixed header words of a one-dimensional
// array with no NULLs. Shared so the two plans below cannot drift apart.
func arrayHeaderInto(buf []byte, elemOID uint32, n int) []byte {
	buf = binary.BigEndian.AppendUint32(buf, 1)         // ndim
	buf = binary.BigEndian.AppendUint32(buf, 0)         // hasnull
	buf = binary.BigEndian.AppendUint32(buf, elemOID)   // element type
	buf = binary.BigEndian.AppendUint32(buf, uint32(n)) // dimension length
	buf = binary.BigEndian.AppendUint32(buf, 1)         // lower bound
	return buf
}

// int8ArrayCodec overrides only the binary encoding of []int64.
type int8ArrayCodec struct{ pgtype.Codec }

func (c int8ArrayCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		if _, ok := value.([]int64); ok {
			return int8ArrayPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type int8ArrayPlan struct{}

func (int8ArrayPlan) Encode(value any, buf []byte) ([]byte, error) {
	vs, ok := value.([]int64)
	if !ok {
		return nil, fmt.Errorf("pgxdrv: int8[] plan got %T", value)
	}
	if vs == nil {
		return nil, nil
	}
	// Every element is a 4-byte length plus 8 bytes, so the size is exact and
	// the buffer grows once.
	need := 20 + len(vs)*12
	if cap(buf)-len(buf) < need {
		grown := make([]byte, len(buf), len(buf)+need)
		copy(grown, buf)
		buf = grown
	}
	buf = arrayHeaderInto(buf, pgtype.Int8OID, len(vs))
	for _, v := range vs {
		buf = binary.BigEndian.AppendUint32(buf, 8)
		buf = binary.BigEndian.AppendUint64(buf, uint64(v))
	}
	return buf, nil
}

// textArrayCodec overrides only the binary encoding of []string.
type textArrayCodec struct{ pgtype.Codec }

func (c textArrayCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		if _, ok := value.([]string); ok {
			return textArrayPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type textArrayPlan struct{}

func (textArrayPlan) Encode(value any, buf []byte) ([]byte, error) {
	vs, ok := value.([]string)
	if !ok {
		return nil, fmt.Errorf("pgxdrv: text[] plan got %T", value)
	}
	if vs == nil {
		return nil, nil
	}
	// Elements are variable-length, so the exact size costs one pass over the
	// lengths — cheaper than growing repeatedly, and it keeps the "one
	// allocation per array" property that is the point of this file.
	need := 20
	for i := range vs {
		need += 4 + len(vs[i])
	}
	if cap(buf)-len(buf) < need {
		grown := make([]byte, len(buf), len(buf)+need)
		copy(grown, buf)
		buf = grown
	}
	buf = arrayHeaderInto(buf, pgtype.TextOID, len(vs))
	for i := range vs {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(vs[i])))
		buf = append(buf, vs[i]...)
	}
	return buf, nil
}

// registerScalarArrays installs the int8[] and text[] fast paths. A server
// missing either type simply keeps pgx's codec for it.
func registerScalarArrays(m *pgtype.Map) {
	if t, ok := m.TypeForOID(pgtype.Int8ArrayOID); ok {
		m.RegisterType(&pgtype.Type{Name: t.Name, OID: t.OID, Codec: int8ArrayCodec{Codec: t.Codec}})
	}
	if t, ok := m.TypeForOID(pgtype.TextArrayOID); ok {
		m.RegisterType(&pgtype.Type{Name: t.Name, OID: t.OID, Codec: textArrayCodec{Codec: t.Codec}})
	}
}
