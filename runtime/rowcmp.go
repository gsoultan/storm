package runtime

import "strings"

// expandRowCmp writes a keyset comparison as an OR-chain, for a back end with
// no row constructor.
//
// `(a, b, c) > (@p1, @p2, @p3)` means, and expands to:
//
//	(a > @p1 OR (a = @p1 AND (b > @p2 OR (b = @p2 AND c > @p3))))
//
// Built from the last column backwards, because each level wraps the one below
// it. The tail is a plain comparison: once every earlier key has tied, the last
// one decides, and a strict inequality there is what stops the page repeating
// the row it just showed.
//
// Each parameter appears twice with the SAME ordinal, so the caller still binds
// n values for n columns and NArg stays the count of VALUES rather than the
// count of mentions. That only holds for a named placeholder; Lowering's
// RowCmpExpand note says why this is not reachable from a bare one.
//
// The cost is the plan, not the text: the OR-chain is sargable on a composite
// index in the same way the tuple form is, but the optimizer has to prove it
// rather than being told, so `storm lint` is where a deep keyset on this target
// should be looked at.
func expandRowCmp(cols []string, op string, ph Placeholder, ord int) string {
	if len(cols) == 0 {
		return "TRUE"
	}
	n := len(cols)
	expr := cols[n-1] + op + ph.text(ord+n)
	for i := n - 2; i >= 0; i-- {
		p := ph.text(ord + i + 1)
		var b strings.Builder
		b.Grow(len(expr) + 2*len(cols[i]) + 2*len(p) + 16)
		b.WriteString("(")
		b.WriteString(cols[i])
		b.WriteString(op)
		b.WriteString(p)
		b.WriteString(" OR (")
		b.WriteString(cols[i])
		b.WriteString(" = ")
		b.WriteString(p)
		b.WriteString(" AND ")
		b.WriteString(expr)
		b.WriteString("))")
		expr = b.String()
	}
	return expr
}
