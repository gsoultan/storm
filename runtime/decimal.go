package runtime

import (
	"encoding/binary"
	"errors"
	"strconv"
)

// Decimal is an exact fixed-point number: Unscaled × 10⁻ᔆᶜᵃˡᵉ.
//
// # Why storm defines its own
//
// Go has no decimal in the standard library, and `runtime/` imports stdlib
// only — a rule that is CI-enforced and load-bearing, because it is what keeps
// a driver's type system out of the scan path. Pulling in a decimal package
// would put a third party's type on every row of every financial table.
//
// So: two machine words, no allocation, exact arithmetic. A caller who wants
// shopspring/decimal converts at the edge, which is one line and their choice.
//
// # The limit, stated rather than hidden
//
// Unscaled is an int64, so a value carries about 18 significant digits. That is
// 9.2 × 10¹⁶ at two decimal places — more currency units than any real ledger
// holds. PostgreSQL's numeric goes to 131,072 digits, so a value CAN exceed
// this, and when it does decoding returns ErrDecimalRange rather than a wrong
// number. Silently truncating money is the one behaviour that must never be an
// option.
//
// A column declared numeric(p,s) with p > 18 is a GENERATION error, so the
// runtime case only arises for unconstrained numeric.
type Decimal struct {
	Unscaled int64
	Scale    int32
}

// ErrDecimalRange means the database value needs more than 18 significant
// digits. Declare the column as text if it genuinely does.
var ErrDecimalRange = errors.New(
	"storm: numeric value exceeds the 18 significant digits a Decimal holds")

// ErrDecimalNaN means the database returned NaN, which has no fixed-point
// representation. It is not a zero and must not become one.
var ErrDecimalNaN = errors.New("storm: numeric is NaN, which has no Decimal representation")

// String renders the value with exactly Scale fraction digits, which is what
// the database displays.
func (d Decimal) String() string {
	neg := d.Unscaled < 0
	u := d.Unscaled
	if neg {
		u = -u
	}
	digits := strconv.FormatInt(u, 10)
	s := int(d.Scale)
	if s <= 0 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	for len(digits) <= s {
		digits = "0" + digits
	}
	out := digits[:len(digits)-s] + "." + digits[len(digits)-s:]
	if neg {
		return "-" + out
	}
	return out
}

// Float64 is for display and comparison only. It is lossy by construction, and
// named so that using it for money is a visible decision rather than an
// accident.
func (d Decimal) Float64() float64 {
	f := float64(d.Unscaled)
	for i := int32(0); i < d.Scale; i++ {
		f /= 10
	}
	for i := d.Scale; i < 0; i++ {
		f *= 10
	}
	return f
}

// PostgreSQL's binary numeric: ndigits, weight, sign, dscale, then ndigits
// base-10000 groups.
const (
	numNaN = 0xC000
	numNeg = 0x4000
)

// DecodeNumeric reads PostgreSQL's binary numeric format.
//
// Base 10000, not base 10: each group is four decimal digits, `weight` is the
// group position of the first one, and `dscale` is how many fraction digits the
// value displays. Reconstructing means placing each group at its own power and
// then scaling to dscale — which is where the overflow check has to be, on
// every step rather than at the end, or the check itself overflows.
func DecodeNumeric(b []byte) (Decimal, error) {
	if len(b) < 8 {
		return Decimal{}, errors.New("storm: numeric wire value is too short")
	}
	ndigits := int(int16(binary.BigEndian.Uint16(b[0:2])))
	weight := int(int16(binary.BigEndian.Uint16(b[2:4])))
	sign := binary.BigEndian.Uint16(b[4:6])
	dscale := int(int16(binary.BigEndian.Uint16(b[6:8])))

	if sign == numNaN {
		return Decimal{}, ErrDecimalNaN
	}
	if ndigits == 0 {
		return Decimal{Scale: int32(dscale)}, nil
	}
	if len(b) < 8+2*ndigits {
		return Decimal{}, errors.New("storm: numeric wire value is truncated")
	}

	// D is the digit groups read as one integer; its last group sits at
	// decimal exponent 4*(weight-ndigits+1).
	var d int64
	for i := 0; i < ndigits; i++ {
		g := int64(binary.BigEndian.Uint16(b[8+2*i : 10+2*i]))
		if d > (1<<62)/10000 {
			return Decimal{}, ErrDecimalRange
		}
		d = d*10000 + g
	}

	exp := 4*(weight-ndigits+1) + dscale
	for ; exp > 0; exp-- {
		if d > (1<<62)/10 {
			return Decimal{}, ErrDecimalRange
		}
		d *= 10
	}
	for ; exp < 0; exp++ {
		// Only trailing zeros are removed here: dscale never drops a digit
		// that carries value, so a non-zero remainder means the wire value did
		// not match its own declared scale.
		if d%10 != 0 {
			return Decimal{}, errors.New("storm: numeric has more precision than its scale")
		}
		d /= 10
	}
	if sign == numNeg {
		d = -d
	}
	return Decimal{Unscaled: d, Scale: int32(dscale)}, nil
}

// EncodeNumeric writes a Decimal in PostgreSQL's binary numeric format.
func EncodeNumeric(d Decimal, buf []byte) []byte {
	u, neg := d.Unscaled, false
	if u < 0 {
		u, neg = -u, true
	}
	scale := int(d.Scale)
	if scale < 0 {
		for ; scale < 0; scale++ {
			u *= 10
		}
		scale = 0
	}

	// Align to a group boundary so the fraction fills whole base-10000 groups.
	pad := (4 - scale%4) % 4
	for i := 0; i < pad; i++ {
		u *= 10
	}
	fracGroups := (scale + pad) / 4

	var groups []uint16
	for u > 0 {
		groups = append(groups, uint16(u%10000))
		u /= 10000
	}
	if len(groups) == 0 {
		buf = binary.BigEndian.AppendUint16(buf, 0)
		buf = binary.BigEndian.AppendUint16(buf, 0)
		buf = binary.BigEndian.AppendUint16(buf, 0)
		return binary.BigEndian.AppendUint16(buf, uint16(d.Scale))
	}
	weight := len(groups) - 1 - fracGroups

	buf = binary.BigEndian.AppendUint16(buf, uint16(len(groups)))
	buf = binary.BigEndian.AppendUint16(buf, uint16(int16(weight)))
	if neg {
		buf = binary.BigEndian.AppendUint16(buf, numNeg)
	} else {
		buf = binary.BigEndian.AppendUint16(buf, 0)
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(d.Scale))
	for i := len(groups) - 1; i >= 0; i-- {
		buf = binary.BigEndian.AppendUint16(buf, groups[i])
	}
	return buf
}

// ParseDecimal reads a decimal from its text form, for declaring literals.
func ParseDecimal(s string) (Decimal, error) {
	neg := false
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := indexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return Decimal{}, errors.New("storm: empty decimal")
	}
	u, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return Decimal{}, ErrDecimalRange
	}
	if neg {
		u = -u
	}
	return Decimal{Unscaled: u, Scale: int32(len(fracPart))}, nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
