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

	// The digit groups read as one integer, with its last group at decimal
	// exponent 4*(weight-ndigits+1), then moved to the declared scale.
	//
	// The trimming happens BEFORE and DURING the accumulation, not after. Doing
	// it after means forming the value at full group precision first, and a
	// numeric whose scale is not a multiple of four carries up to three padding
	// zeros past it — so 123456789.987654321, which fits a Decimal exactly,
	// was rejected as out of range while its own encoding (and pgx's, which is
	// byte-identical here) said otherwise.
	exp := 4*(weight-ndigits+1) + dscale

	// Whole trailing groups the declared scale does not reach. They must be
	// zeros: dscale never drops a digit that carries value.
	if exp < 0 {
		drop := min((-exp)/4, ndigits)
		for i := ndigits - drop; i < ndigits; i++ {
			if binary.BigEndian.Uint16(b[8+2*i:10+2*i]) != 0 {
				return Decimal{}, errNumericPrecision
			}
		}
		ndigits -= drop
		exp += 4 * drop
	}
	if ndigits == 0 {
		return Decimal{Scale: int32(dscale)}, nil
	}

	// What is left is a sub-group shift of at most three digits, applied to the
	// last group as it is folded in rather than to the whole value.
	rem := 0
	if exp < 0 {
		rem, exp = -exp, 0
	}

	var d int64
	whole := ndigits
	if rem > 0 {
		whole--
	}
	for i := 0; i < whole; i++ {
		g := int64(binary.BigEndian.Uint16(b[8+2*i : 10+2*i]))
		if d > (1<<62)/10000 {
			return Decimal{}, ErrDecimalRange
		}
		d = d*10000 + g
	}
	if rem > 0 {
		last := int64(binary.BigEndian.Uint16(b[8+2*(ndigits-1) : 10+2*(ndigits-1)]))
		if last%pow10[rem] != 0 {
			return Decimal{}, errNumericPrecision
		}
		mul := pow10[4-rem]
		if d > (1<<62)/mul {
			return Decimal{}, ErrDecimalRange
		}
		d = d*mul + last/pow10[rem]
	}

	for ; exp > 0; exp-- {
		if d > (1<<62)/10 {
			return Decimal{}, ErrDecimalRange
		}
		d *= 10
	}
	if sign == numNeg {
		d = -d
	}
	return Decimal{Unscaled: d, Scale: int32(dscale)}, nil
}

var errNumericPrecision = errors.New("storm: numeric has more precision than its scale")

// EncodeNumeric writes a Decimal in PostgreSQL's binary numeric format.
func EncodeNumeric(d Decimal, buf []byte) []byte {
	u, neg := d.Unscaled, false
	if u < 0 {
		// math.MinInt64 has no positive counterpart, so negating it is still
		// negative. It cannot be reached through ParseDecimal, which rejects
		// anything that does not fit, but EncodeNumeric is exported and a
		// caller can build a Decimal by hand.
		if u == minInt64 {
			return appendZeroNumeric(buf, d.Scale)
		}
		u, neg = -u, true
	}

	// shift is how many decimal places the unscaled value must move LEFT
	// before it is grouped: to fill whole base-10000 groups when the scale is
	// not a multiple of four, and to restore the trailing zeros a negative
	// scale stands for.
	//
	// It is never applied by MULTIPLYING u, which is what this function used
	// to do. `u *= 10` up to three times overflows int64 for any value past
	// about 9.2e15, u went negative, the digit loop never ran, and the encoder
	// emitted the ZERO encoding — so 123456789.987654321 was written to the
	// database as 0.000000000, silently, on insert and in predicates alike.
	// The round-trip test existed; every value in it was small enough to miss.
	scale := int(d.Scale)
	shift := 0
	if scale < 0 {
		shift, scale = -scale, 0
	} else if pad := (4 - scale%4) % 4; pad != 0 {
		shift = pad
		scale += pad
	}
	fracGroups := scale / 4

	// A shift of four or more is whole groups of zeros at the least
	// significant end; only the remainder moves digits across a boundary.
	zeroGroups, residual := shift/4, shift%4
	if u == 0 {
		return appendZeroNumeric(buf, d.Scale)
	}

	groups := make([]uint16, 0, 24)
	for i := 0; i < zeroGroups; i++ {
		groups = append(groups, 0)
	}
	// The first real group takes the low 4-residual digits of u and moves them
	// up by residual places, which is the whole trick: it multiplies at most
	// 9,999 rather than the entire value.
	if residual != 0 {
		lowDiv := pow10[4-residual]
		groups = append(groups, uint16(u%lowDiv)*uint16(pow10[residual]))
		u /= lowDiv
	}
	for u > 0 {
		groups = append(groups, uint16(u%10000))
		u /= 10000
	}
	weight := len(groups) - 1 - fracGroups

	// PostgreSQL's canonical form carries no trailing zero groups. They are at
	// the FRONT here, because groups are built least-significant first, and
	// dropping them cannot change the value: weight names the most significant
	// group and is already fixed above.
	i := 0
	for i < len(groups) && groups[i] == 0 {
		i++
	}
	groups = groups[i:]
	if len(groups) == 0 {
		return appendZeroNumeric(buf, d.Scale)
	}

	buf = binary.BigEndian.AppendUint16(buf, uint16(len(groups)))
	buf = binary.BigEndian.AppendUint16(buf, uint16(int16(weight)))
	if neg {
		buf = binary.BigEndian.AppendUint16(buf, numNeg)
	} else {
		buf = binary.BigEndian.AppendUint16(buf, 0)
	}
	buf = binary.BigEndian.AppendUint16(buf, dscaleOf(d.Scale))
	for i := len(groups) - 1; i >= 0; i-- {
		buf = binary.BigEndian.AppendUint16(buf, groups[i])
	}
	return buf
}

const minInt64 = -1 << 63

// pow10 indexes the four powers a residual shift can need.
var pow10 = [5]int64{1, 10, 100, 1000, 10000}

// dscaleOf is the display scale PostgreSQL is told. It is never negative on
// the wire: a negative Scale means trailing zeros, which the digits carry.
func dscaleOf(scale int32) uint16 {
	if scale < 0 {
		return 0
	}
	return uint16(scale)
}

// appendZeroNumeric writes the number zero: no digits, weight zero, positive.
func appendZeroNumeric(buf []byte, scale int32) []byte {
	buf = binary.BigEndian.AppendUint16(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 0)
	return binary.BigEndian.AppendUint16(buf, dscaleOf(scale))
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
