package storm

import "reflect"

// reflectString reads a named string type — an enum declared as
// `type Status string` — so a declared literal can carry its value.
//
// Reflection at DECLARATION time, in the generator, on the developer's
// machine. The ban that matters is reflection in the query path (runtime/),
// and this is on the other side of it.
func reflectString(v any) (string, bool) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "", false
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return rv.String(), true
	}
	return "", false
}

// isFieldPtr reports whether v is a pointer INTO a struct rather than a value.
//
// It decides whether the right-hand side of a comparison is another column or
// a literal, and it has to be conservative: a *string that is really a nullable
// column's field pointer and a *string someone passed as a value are the same
// type. A pointer is treated as a field reference, which is what every field
// pointer in this API already is; pass storm.Lit(x) to force the other reading.
func isFieldPtr(v any) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(Term); ok {
		return false
	}
	return reflect.ValueOf(v).Kind() == reflect.Pointer
}
