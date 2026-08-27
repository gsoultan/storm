package pgxdrv

import (
	"fmt"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgtype"
)

// Binding a runtime.Decimal as a numeric parameter.
//
// pgx has never heard of storm's Decimal, and it must not have to: teaching the
// runtime about pgx is the coupling this adapter exists to prevent. So the
// knowledge lives here, in the one package allowed to name a pgx type, exactly
// as the uuid[] encoder does.
//
// The alternative — binding the value as text and letting Postgres coerce it —
// works until it does not: an untyped parameter takes its type from context,
// and `WHERE amount = $1` in a prepared statement resolves that context before
// the value exists. Encoding it as numeric means the parameter IS a numeric.

type decimalCodec struct{ pgtype.Codec }

func (c decimalCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format == pgtype.BinaryFormatCode {
		switch value.(type) {
		case runtime.Decimal, *runtime.Decimal:
			return decimalPlan{}
		}
	}
	return c.Codec.PlanEncode(m, oid, format, value)
}

type decimalPlan struct{}

func (decimalPlan) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case runtime.Decimal:
		return runtime.EncodeNumeric(v, buf), nil
	case *runtime.Decimal:
		if v == nil {
			return nil, nil // SQL NULL
		}
		return runtime.EncodeNumeric(*v, buf), nil
	}
	return nil, fmt.Errorf("pgxdrv: numeric plan got %T", value)
}

func registerDecimal(m *pgtype.Map) {
	t, ok := m.TypeForOID(pgtype.NumericOID)
	if !ok {
		return
	}
	m.RegisterType(&pgtype.Type{Name: t.Name, OID: t.OID, Codec: decimalCodec{Codec: t.Codec}})
}
