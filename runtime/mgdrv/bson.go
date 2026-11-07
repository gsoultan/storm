package mgdrv

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/bsondec"
)

// BSON, the subset this driver writes and reads. Append-style over a []byte so
// a command is built into one buffer the connection already owns.

const (
	tDouble   = 0x01
	tString   = 0x02
	tDocument = 0x03
	tArray    = 0x04
	tBinary   = 0x05
	tUndef    = 0x06
	tObjectID = 0x07
	tBool     = 0x08
	tDateTime = 0x09
	tNull     = 0x0A
	tRegex    = 0x0B
	tDBPtr    = 0x0C
	tJS       = 0x0D
	tSymbol   = 0x0E
	tJSScope  = 0x0F
	tInt32    = 0x10
	tTime     = 0x11
	tInt64    = 0x12
	tDecimal  = 0x13
	tMinKey   = 0xFF
	tMaxKey   = 0x7F
)

var errMalformed = errors.New("mgdrv: malformed BSON from the server")

// ErrArg is an argument of a Go type this driver has no BSON form for.
var ErrArg = errors.New("mgdrv: unsupported argument type")

func appendI32(b []byte, v int32) []byte { return binary.LittleEndian.AppendUint32(b, uint32(v)) }
func appendI64(b []byte, v int64) []byte { return binary.LittleEndian.AppendUint64(b, uint64(v)) }

// startDoc reserves a document's length and returns where it is.
func startDoc(b []byte) ([]byte, int) { return append(b, 0, 0, 0, 0), len(b) }

// endDoc terminates the document opened at at and writes its length.
func endDoc(b []byte, at int) []byte {
	b = append(b, 0)
	binary.LittleEndian.PutUint32(b[at:], uint32(len(b)-at))
	return b
}

func appendName(b []byte, t byte, name string) []byte {
	b = append(b, t)
	b = append(b, name...)
	return append(b, 0)
}

func appendStr(b []byte, name, v string) []byte {
	b = appendName(b, tString, name)
	b = appendI32(b, int32(len(v)+1))
	b = append(b, v...)
	return append(b, 0)
}

func appendInt32(b []byte, name string, v int32) []byte {
	return appendI32(appendName(b, tInt32, name), v)
}

func appendInt64(b []byte, name string, v int64) []byte {
	return appendI64(appendName(b, tInt64, name), v)
}

func appendBool(b []byte, name string, v bool) []byte {
	b = appendName(b, tBool, name)
	if v {
		return append(b, 1)
	}
	return append(b, 0)
}

func appendBinary(b []byte, name string, sub byte, v []byte) []byte {
	b = appendName(b, tBinary, name)
	b = appendI32(b, int32(len(v)))
	b = append(b, sub)
	return append(b, v...)
}

// appendDecimal writes a fixed-point decimal as a Decimal128: the unscaled
// value is the coefficient and the negated scale the exponent, so the value is
// exact and keeps its scale — 1.50 stays 1.50.
func appendDecimal(b []byte, name string, d runtime.Decimal) []byte {
	b = appendName(b, tDecimal, name)
	u := d.Unscaled
	var sign uint64
	if u < 0 {
		sign = 1 << 63
		u = -u // MinInt64 is out of range for 18 digits and never produced
	}
	hi := sign | uint64(int64(6176)-int64(d.Scale))<<49
	b = appendI64(b, u)
	return appendI64(b, int64(hi))
}

// argName is the element name of the i-th array member: arrays are documents
// keyed "0", "1", ….
func argName(i int) string {
	if i < len(smallNames) {
		return smallNames[i]
	}
	return strconv.Itoa(i)
}

var smallNames = func() []string {
	s := make([]string, 64)
	for i := range s {
		s[i] = strconv.Itoa(i)
	}
	return s
}()

// appendValue writes one bound argument.
//
// A type switch, not reflection: the generated code passes exactly the types
// a column's Go representation has, plus their pointers (a bulk load binds a
// pointer per field, because boxing one does not allocate) and the slices an
// In takes.
func appendValue(b []byte, name string, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return appendName(b, tNull, name), nil
	case string:
		return appendStr(b, name, x), nil
	case *string:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendStr(b, name, *x), nil
	case bool:
		return appendBool(b, name, x), nil
	case *bool:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendBool(b, name, *x), nil
	case int:
		return appendInt64(b, name, int64(x)), nil
	case int16:
		return appendInt32(b, name, int32(x)), nil
	case *int16:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendInt32(b, name, int32(*x)), nil
	case int32:
		return appendInt32(b, name, x), nil
	case *int32:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendInt32(b, name, *x), nil
	case int64:
		return appendInt64(b, name, x), nil
	case *int64:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendInt64(b, name, *x), nil
	case float32:
		return appendI64(appendName(b, tDouble, name), int64(math.Float64bits(float64(x)))), nil
	case *float32:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendValue(b, name, *x)
	case float64:
		return appendI64(appendName(b, tDouble, name), int64(math.Float64bits(x))), nil
	case *float64:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendValue(b, name, *x)
	case time.Time:
		return appendI64(appendName(b, tDateTime, name), x.UnixMilli()), nil
	case *time.Time:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendI64(appendName(b, tDateTime, name), x.UnixMilli()), nil
	case [16]byte:
		return appendBinary(b, name, 4, x[:]), nil
	case *[16]byte:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendBinary(b, name, 4, x[:]), nil
	case []byte:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendBinary(b, name, 0, x), nil
	case *[]byte:
		if x == nil || *x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendBinary(b, name, 0, *x), nil
	case runtime.Decimal:
		return appendDecimal(b, name, x), nil
	case *runtime.Decimal:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendDecimal(b, name, *x), nil
	case []string:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendStr(b, argName(i), x[i]), nil
		})
	case []int64:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendInt64(b, argName(i), x[i]), nil
		})
	case []int32:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendInt32(b, argName(i), x[i]), nil
		})
	case []int16:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendInt32(b, argName(i), int32(x[i])), nil
		})
	case []float64:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendValue(b, argName(i), x[i])
		})
	case []float32:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendValue(b, argName(i), x[i])
		})
	case []bool:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendBool(b, argName(i), x[i]), nil
		})
	case [][16]byte:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendBinary(b, argName(i), 4, x[i][:]), nil
		})
	case []time.Time:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendValue(b, argName(i), x[i])
		})
	case bsondec.Raw:
		if len(x) == 0 {
			return appendName(b, tNull, name), nil
		}
		b = append(b, x[0])
		b = append(append(b, name...), 0)
		return append(b, x[1:]...), nil
	case []bsondec.Raw:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendValue(b, argName(i), x[i])
		})
	case runtime.JSON:
		return appendJSON(b, name, x)
	case *runtime.JSON:
		if x == nil {
			return appendName(b, tNull, name), nil
		}
		return appendJSON(b, name, *x)
	case []runtime.Decimal:
		return appendArray(b, name, len(x), func(b []byte, i int) ([]byte, error) {
			return appendDecimal(b, argName(i), x[i]), nil
		})
	}
	return b, ErrArg
}

func appendArray(b []byte, name string, n int, each func([]byte, int) ([]byte, error)) ([]byte, error) {
	b = appendName(b, tArray, name)
	b, at := startDoc(b)
	var err error
	for i := 0; i < n; i++ {
		if b, err = each(b, i); err != nil {
			return b, err
		}
	}
	return endDoc(b, at), nil
}

// ---- reading ----

// valueSize is how many bytes the value of type t occupies at the start of b,
// or -1 when b is too short or t is unknown.
func valueSize(t byte, b []byte) int {
	need := func(n int) int {
		if n < 0 || n > len(b) {
			return -1
		}
		return n
	}
	switch t {
	case tDouble, tDateTime, tInt64, tTime:
		return need(8)
	case tInt32:
		return need(4)
	case tDecimal:
		return need(16)
	case tBool:
		return need(1)
	case tNull, tUndef, tMinKey, tMaxKey:
		return 0
	case tObjectID:
		return need(12)
	case tString, tJS, tSymbol:
		if len(b) < 4 {
			return -1
		}
		return need(4 + int(int32(binary.LittleEndian.Uint32(b))))
	case tDocument, tArray, tJSScope:
		if len(b) < 4 {
			return -1
		}
		return need(int(int32(binary.LittleEndian.Uint32(b))))
	case tBinary:
		if len(b) < 4 {
			return -1
		}
		return need(5 + int(int32(binary.LittleEndian.Uint32(b))))
	case tRegex:
		n := 0
		for k := 0; k < 2; k++ {
			for n < len(b) && b[n] != 0 {
				n++
			}
			if n >= len(b) {
				return -1
			}
			n++
		}
		return n
	case tDBPtr:
		if len(b) < 4 {
			return -1
		}
		return need(4 + int(int32(binary.LittleEndian.Uint32(b))) + 12)
	}
	return -1
}

// elements walks a document, calling fn for each element until it returns
// false. It reports a malformed document.
func elements(doc []byte, fn func(t byte, name string, v []byte) bool) error {
	if len(doc) < 5 {
		return errMalformed
	}
	n := int(int32(binary.LittleEndian.Uint32(doc)))
	if n != len(doc) || doc[n-1] != 0 {
		return errMalformed
	}
	i := 4
	for i < n-1 {
		t := doc[i]
		i++
		j := i
		for j < n && doc[j] != 0 {
			j++
		}
		// n-1, not n: the document's own terminator is not the end of a
		// name. A name running into it left i at n, and doc[i:n-1] below
		// panicked on a reply a corrupt or hostile server sent.
		if j >= n-1 {
			return errMalformed
		}
		name := doc[i:j]
		i = j + 1
		sz := valueSize(t, doc[i:n-1])
		if sz < 0 {
			return errMalformed
		}
		if !fn(t, string(name), doc[i:i+sz]) {
			return nil
		}
		i += sz
	}
	return nil
}

// lookup finds one top-level field.
func lookup(doc []byte, key string) (byte, []byte, bool) {
	var ft byte
	var fv []byte
	found := false
	_ = elements(doc, func(t byte, name string, v []byte) bool {
		if name == key {
			ft, fv, found = t, v, true
			return false
		}
		return true
	})
	return ft, fv, found
}

// asInt64 reads any BSON number as an int64.
func asInt64(t byte, v []byte) (int64, bool) {
	switch t {
	case tInt32:
		return int64(int32(binary.LittleEndian.Uint32(v))), true
	case tInt64:
		return int64(binary.LittleEndian.Uint64(v)), true
	case tDouble:
		return int64(math.Float64frombits(binary.LittleEndian.Uint64(v))), true
	case tBool:
		if v[0] != 0 {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// asString reads a BSON string's text.
func asString(t byte, v []byte) (string, bool) {
	if t != tString || len(v) < 5 {
		return "", false
	}
	return string(v[4 : len(v)-1]), true
}

func intField(doc []byte, key string) int64 {
	t, v, ok := lookup(doc, key)
	if !ok {
		return 0
	}
	n, _ := asInt64(t, v)
	return n
}

func strField(doc []byte, key string) string {
	t, v, ok := lookup(doc, key)
	if !ok {
		return ""
	}
	s, _ := asString(t, v)
	return s
}

// appendJSON binds a json or jsonb value as the BSON value its text denotes:
// an object as a document with its keys in written order, an array as an
// array, a number as int32, int64 or double — the narrowest that holds it
// exactly. Stored that way rather than as text, a field inside is queryable
// as what it is. An empty or nil value binds null, as the SQL back ends'
// JSON.OrNull does.
func appendJSON(b []byte, name string, j runtime.JSON) ([]byte, error) {
	if len(j) == 0 {
		return appendName(b, tNull, name), nil
	}
	d := json.NewDecoder(strings.NewReader(string(j)))
	d.UseNumber()
	b, err := jsonValue(b, name, d, 0)
	if err != nil {
		return b, err
	}
	if _, err := d.Token(); err != io.EOF {
		return b, errJSON
	}
	return b, nil
}

var errJSON = errors.New("mgdrv: a json value is not one JSON document")

// maxJSONDepth bounds the recursion on a value the caller supplied. MongoDB
// refuses documents nested deeper than 100 levels anyway.
const maxJSONDepth = 100

func jsonValue(b []byte, name string, d *json.Decoder, depth int) ([]byte, error) {
	if depth > maxJSONDepth {
		return b, errJSON
	}
	tok, err := d.Token()
	if err != nil {
		return b, errJSON
	}
	switch v := tok.(type) {
	case json.Delim:
		t := byte(tDocument)
		if v == '[' {
			t = tArray
		} else if v != '{' {
			return b, errJSON
		}
		b = appendName(b, t, name)
		b, at := startDoc(b)
		for i := 0; d.More(); i++ {
			key := argName(i)
			if t == tDocument {
				k, err := d.Token()
				ks, ok := k.(string)
				if err != nil || !ok || strings.IndexByte(ks, 0) >= 0 {
					return b, errJSON
				}
				key = ks
			}
			if b, err = jsonValue(b, key, d, depth+1); err != nil {
				return b, err
			}
		}
		if _, err := d.Token(); err != nil {
			return b, errJSON
		}
		return endDoc(b, at), nil
	case string:
		return appendStr(b, name, v), nil
	case bool:
		return appendBool(b, name, v), nil
	case nil:
		return appendName(b, tNull, name), nil
	case json.Number:
		s := string(v)
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				if n >= math.MinInt32 && n <= math.MaxInt32 {
					return appendInt32(b, name, int32(n)), nil
				}
				return appendInt64(b, name, n), nil
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return b, errJSON
		}
		return appendValue(b, name, f)
	}
	return b, errJSON
}

// appendFields writes one document's fields, nesting a dotted name — a
// composite key's "_id.tenant" — into the subdocument its head names. Fields
// that share a head are gathered under it in the order they first appear,
// which for a key is key order.
func appendFields(b []byte, cols []string, vals []any) ([]byte, error) {
	done := make([]bool, len(cols))
	var err error
	for i, col := range cols {
		if done[i] || i >= len(vals) {
			continue
		}
		head, _, nested := strings.Cut(col, ".")
		if !nested {
			if b, err = appendValue(b, col, vals[i]); err != nil {
				return b, errors.New("mgdrv: field " + col + ": unsupported Go type")
			}
			continue
		}
		b = appendName(b, tDocument, head)
		var at int
		b, at = startDoc(b)
		for j := i; j < len(cols) && j < len(vals); j++ {
			h, rest, ok := strings.Cut(cols[j], ".")
			if !ok || h != head {
				continue
			}
			if b, err = appendValue(b, rest, vals[j]); err != nil {
				return b, errors.New("mgdrv: field " + cols[j] + ": unsupported Go type")
			}
			done[j] = true
		}
		b = endDoc(b, at)
	}
	return b, nil
}
