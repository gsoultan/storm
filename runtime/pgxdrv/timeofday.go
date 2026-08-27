package pgxdrv

import (
	"fmt"

	"github.com/gsoultan/raorm/runtime"
	"github.com/jackc/pgx/v5/pgtype"
)

// runtime.TimeOfDay parameter encoding.
//
// Same bridge as Interval and Decimal: runtime/ is stdlib-only, so the type
// cannot implement a pgx interface, and this is the one package allowed to
// name one.
//
// It matters more here than it looks. TimeOfDay's underlying kind is int64,
// so WITHOUT this plan pgx encodes a bound value as int8 and PostgreSQL
// refuses the comparison — or, worse on a driver that coerces, compares a
// microsecond count against a time. The generated builder keeps the Go type
// intact all the way from the arena to here precisely so this plan can fire.

type timeOfDayCodec struct{ pgtype.Codec }

func (c timeOfDayCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		switch value.(type) {
		case runtime.TimeOfDay, *runtime.TimeOfDay:
			return timeOfDayPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type timeOfDayPlan struct{}

func (timeOfDayPlan) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case runtime.TimeOfDay:
		return runtime.EncodeTimeOfDay(v, buf), nil
	case *runtime.TimeOfDay:
		if v == nil {
			return nil, nil // SQL NULL
		}
		return runtime.EncodeTimeOfDay(*v, buf), nil
	}
	return nil, fmt.Errorf("pgxdrv: time plan got %T", value)
}

func registerTimeOfDay(m *pgtype.Map) {
	t, ok := m.TypeForOID(pgtype.TimeOID)
	if !ok {
		return
	}
	m.RegisterType(&pgtype.Type{
		Name:  t.Name,
		OID:   t.OID,
		Codec: timeOfDayCodec{Codec: t.Codec},
	})
}
