package pgxdrv

import (
	"fmt"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgtype"
)

// Binding a runtime.TstzRange as a tstzrange parameter.
//
// Same reasoning as the decimal codec: pgx has never heard of storm's range
// type and must not have to, so the knowledge lives in the one package allowed
// to name a pgx type. Encoding it as a real tstzrange also means the parameter
// IS one — `WHERE during && $1` in a prepared statement resolves the
// parameter's type before the value exists, so an untyped text value would be
// resolved as text and the operator would not be found.

type tstzRangeCodec struct{ pgtype.Codec }

type tstzRangePlan struct{}

func (c tstzRangeCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		switch value.(type) {
		case runtime.TstzRange, *runtime.TstzRange:
			return tstzRangePlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

func (tstzRangePlan) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case runtime.TstzRange:
		return runtime.EncodeTstzRange(v, buf), nil
	case *runtime.TstzRange:
		if v == nil {
			return nil, nil // SQL NULL
		}
		return runtime.EncodeTstzRange(*v, buf), nil
	}
	return nil, fmt.Errorf("pgxdrv: tstzrange plan got %T", value)
}

func registerTstzRange(m *pgtype.Map) {
	t, ok := m.TypeForOID(pgtype.TstzrangeOID)
	if !ok {
		return
	}
	m.RegisterType(&pgtype.Type{Name: t.Name, OID: t.OID, Codec: tstzRangeCodec{Codec: t.Codec}})
}
