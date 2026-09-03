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

// Singular is the singular form of a table name, for deriving that table's
// foreign-key column: "posts" gives "post", so the column is "post_id".
//
// Here rather than in codegen for the reason GoName is here. The rule feeds
// two things that must agree exactly — the column a join table declares, and
// the column the generated loader filters on — and a second copy is how they
// drift into naming different columns.
//
// Deliberately small. It undoes the pluralisation storm itself applies and
// nothing more; a model whose name does not survive the round trip pins its
// table explicitly, which is the escape that already exists.
func Singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "ches"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	}
	return s
}
