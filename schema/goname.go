package schema

import "strings"

// GoName is the exported Go field name for a column.
//
// The ONE implementation. It used to be spelled twice — once in codegen for the
// struct field, once in the root package for a declared aggregate's grouping
// name — and the two disagreed on initialisms: `customer_id` became CustomerID
// in the generated struct and CustomerId in the declaration, so the scanner
// assigned to a field that did not exist. It lives here because `schema` is the
// one package both sides already import.
func GoName(col string) string {
	parts := strings.Split(col, "_")
	for i, p := range parts {
		switch p {
		case "id":
			parts[i] = "ID"
		case "url", "uri", "ip", "api":
			parts[i] = strings.ToUpper(p)
		default:
			if p != "" {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
	}
	return strings.Join(parts, "")
}
