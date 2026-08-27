package pgxdrv

import (
	"fmt"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgtype"
)

// runtime.Interval parameter encoding.
//
// runtime/ is stdlib-only, so Interval cannot implement a pgx interface — the
// bridge lives here, the one package allowed to name a pgx type. Same shape as
// the uuid[] fast path: wrap the installed codec, add a plan for our type,
// delegate everything else.

type intervalCodec struct{ pgtype.Codec }

func (c intervalCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		switch value.(type) {
		case runtime.Interval, *runtime.Interval:
			return intervalPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type intervalPlan struct{}

func (intervalPlan) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case runtime.Interval:
		return runtime.EncodeInterval(v, buf), nil
	case *runtime.Interval:
		if v == nil {
			return nil, nil // SQL NULL
		}
		return runtime.EncodeInterval(*v, buf), nil
	}
	return nil, fmt.Errorf("pgxdrv: interval plan got %T", value)
}

// registerInterval installs the bridge; called from RegisterFastArrays so one
// registration call covers every storm type.
func registerInterval(m *pgtype.Map) {
	t, ok := m.TypeForOID(pgtype.IntervalOID)
	if !ok {
		return
	}
	m.RegisterType(&pgtype.Type{
		Name:  t.Name,
		OID:   t.OID,
		Codec: intervalCodec{Codec: t.Codec},
	})
}
