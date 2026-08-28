package orders

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/gsoultan/storm"
)

// Money arithmetic on storm.Decimal (an exact {Unscaled, Scale} pair).
//
// These are deliberately tiny and deliberately not float64: a float64 cannot
// represent 0.10, and an order total that rounds is a defect rather than a
// tolerance. A real service would use a decimal library; the point here is
// only that the value never becomes a float on the way to or from Postgres.

func rescale(d storm.Decimal, scale int32) storm.Decimal {
	for d.Scale < scale {
		d.Unscaled *= 10
		d.Scale++
	}
	return d
}

func addDecimal(a, b storm.Decimal) storm.Decimal {
	scale := max(a.Scale, b.Scale)
	a, b = rescale(a, scale), rescale(b, scale)
	return storm.Decimal{Unscaled: a.Unscaled + b.Unscaled, Scale: scale}
}

func mulDecimal(d storm.Decimal, n int32) storm.Decimal {
	return storm.Decimal{Unscaled: d.Unscaled * int64(n), Scale: d.Scale}
}

func newID() [16]byte {
	// uuidv7-ish is the database's job (id has a default); these are the ids
	// the unit of work needs to KNOW before the rows exist, so it can wire the
	// foreign keys up before either row is written.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return b
}

func formatID(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func parseID(s string) ([16]byte, error) {
	var out [16]byte
	clean := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			clean = append(clean, s[i])
		}
	}
	if len(clean) != 32 {
		return out, fmt.Errorf("%q is not a uuid", s)
	}
	b, err := hex.DecodeString(string(clean))
	if err != nil {
		return out, fmt.Errorf("%q is not a uuid: %w", s, err)
	}
	copy(out[:], b)
	return out, nil
}
