package qcache

import "github.com/gsoultan/storm/runtime"

// hitRows replays a cached entry as the byte shape of runtime.Rows.
type hitRows struct {
	raw    [][][]byte
	vals   [][]any
	values bool
	at     int
}

func (h *hitRows) len() int {
	if h.values {
		return len(h.vals)
	}
	return len(h.raw)
}

func (h *hitRows) Next() bool {
	if h.at >= h.len() {
		h.at = h.len() + 1
		return false
	}
	h.at++
	return true
}

func (h *hitRows) RawValues() [][]byte {
	if h.values || h.at < 1 || h.at > len(h.raw) {
		return nil
	}
	return h.raw[h.at-1]
}

func (h *hitRows) Close()     {}
func (h *hitRows) Err() error { return nil }

// rows hands the entry back in the shape it was recorded in. A value-shaped
// entry must satisfy runtime.ValueRows, or the generated package's
// runtime.AsValueRows would refuse it; a byte-shaped one must NOT, for the
// same reason in the other direction.
func (h *hitRows) rows() runtime.Rows {
	if h.values {
		return (*valueHit)(h)
	}
	return h
}

// valueHit is hitRows with Values: the value shape, runtime.ValueRows.
type valueHit hitRows

func (v *valueHit) Next() bool          { return (*hitRows)(v).Next() }
func (v *valueHit) RawValues() [][]byte { return nil }
func (v *valueHit) Close()              {}
func (v *valueHit) Err() error          { return nil }

func (v *valueHit) Values() []any {
	if v.at < 1 || v.at > len(v.vals) {
		return nil
	}
	return v.vals[v.at-1]
}
