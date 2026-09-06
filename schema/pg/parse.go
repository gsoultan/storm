package pg

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Postgres hands constraint and index definitions back as normalised SQL text.
// These parsers pull the structure back out. They are deliberately small and
// only handle what storm emits; anything unrecognised is preserved verbatim so
// a diff reports it rather than silently dropping it.

// stripCheck turns "CHECK ((age > 0))" into "age > 0".
func stripCheck(def string) string {
	s := strings.TrimSpace(def)
	s = strings.TrimPrefix(s, "CHECK ")
	return unwrap(strings.TrimSpace(s))
}

// refColumns pulls the referenced columns out of a FOREIGN KEY definition:
// FOREIGN KEY (a, b) REFERENCES other(x, y) ON DELETE CASCADE
func refColumns(def string) []string {
	i := strings.Index(def, "REFERENCES ")
	if i < 0 {
		return nil
	}
	rest := def[i+len("REFERENCES "):]
	open := strings.Index(rest, "(")
	if open < 0 {
		return nil
	}
	depth, end := 0, -1
	for j := open; j < len(rest); j++ {
		switch rest[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	return splitList(rest[open+1 : end])
}

// parseExclude reads: EXCLUDE USING gist (room WITH =, period WITH &&) WHERE (...)
func parseExclude(name, def string) *schema.Exclude {
	ex := &schema.Exclude{Name: name, Method: "gist"}
	if i := strings.Index(def, "USING "); i >= 0 {
		rest := def[i+len("USING "):]
		if sp := strings.IndexByte(rest, ' '); sp > 0 {
			ex.Method = rest[:sp]
		}
	}
	body, after := balanced(def, strings.Index(def, "("))
	for _, part := range splitList(body) {
		if j := strings.LastIndex(part, " WITH "); j >= 0 {
			col := strings.TrimSpace(part[:j])
			isExpr := strings.ContainsAny(col, "()")
			if isExpr {
				col = unwrap(col)
			} else {
				col = unquote(col)
			}
			ex.Parts = append(ex.Parts, schema.ExcludePart{
				Column:   col,
				Expr:     isExpr,
				Operator: strings.TrimSpace(part[j+len(" WITH "):]),
			})
		}
	}
	if k := strings.Index(after, "WHERE "); k >= 0 {
		ex.Where = unwrap(strings.TrimSpace(after[k+len("WHERE "):]))
	}
	return ex
}

// indexDef is what pg_get_indexdef says about an index beyond its name,
// uniqueness and access method: the keys, and the optional clauses that follow
// them in the order PostgreSQL prints them —
//
//	(keys) INCLUDE (...) NULLS NOT DISTINCT WITH (...) WHERE (...)
type indexDef struct {
	Columns          []schema.IndexColumn
	Include          []string
	NullsNotDistinct bool
	With             []schema.StorageParam
	Where            string
}

// parseIndexDef reads a CREATE INDEX statement back into the IR.
func parseIndexDef(def string) indexDef {
	var d indexDef
	u := strings.Index(def, " USING ")
	if u < 0 {
		return d
	}
	open := strings.Index(def[u:], "(")
	if open < 0 {
		return d
	}
	body, after := balanced(def, u+open)
	for _, part := range splitList(body) {
		d.Columns = append(d.Columns, parseIndexKey(part))
	}

	after = strings.TrimSpace(after)
	if strings.HasPrefix(after, "INCLUDE ") {
		inner, rest := balanced(after, strings.Index(after, "("))
		d.Include = splitList(inner)
		after = strings.TrimSpace(rest)
	}
	if strings.HasPrefix(after, "NULLS NOT DISTINCT") {
		d.NullsNotDistinct = true
		after = strings.TrimSpace(strings.TrimPrefix(after, "NULLS NOT DISTINCT"))
	}
	if strings.HasPrefix(after, "WITH ") {
		inner, rest := balanced(after, strings.Index(after, "("))
		for _, kv := range splitList(inner) {
			name, value, _ := strings.Cut(kv, "=")
			d.With = append(d.With, schema.StorageParam{
				Name:  strings.TrimSpace(name),
				Value: unquoteLit(strings.TrimSpace(value)),
			})
		}
		after = strings.TrimSpace(rest)
	}
	if k := strings.Index(after, "WHERE "); k >= 0 {
		d.Where = unwrap(strings.TrimSpace(after[k+len("WHERE "):]))
	}
	return d
}

// parseIndexKey reads one key. The trailing clauses come off in reverse print
// order — NULLS placement, direction, operator class, collation — and what is
// left is the column or expression.
//
// Only the non-default placement is printed: an ascending key's NULLS LAST and
// a descending key's NULLS FIRST are the defaults, and PostgreSQL does not
// print what it would have done anyway. So a DESC key reads back with neither
// flag, and that is the shape the model is held to.
func parseIndexKey(part string) schema.IndexColumn {
	c := schema.IndexColumn{}
	p := strings.TrimSpace(part)
	switch {
	case strings.HasSuffix(p, " NULLS LAST"):
		c.NullsLast = true
		p = strings.TrimSpace(strings.TrimSuffix(p, " NULLS LAST"))
	case strings.HasSuffix(p, " NULLS FIRST"):
		c.NullsFirst = true
		p = strings.TrimSpace(strings.TrimSuffix(p, " NULLS FIRST"))
	}
	switch {
	case strings.HasSuffix(p, " DESC"):
		c.Desc = true
		p = strings.TrimSpace(strings.TrimSuffix(p, " DESC"))
	case strings.HasSuffix(p, " ASC"):
		p = strings.TrimSpace(strings.TrimSuffix(p, " ASC"))
	}
	// The operator class is the last token when there is one. Every built-in
	// and contrib class ends in _ops. One from a schema outside the search
	// path is printed qualified — public.gin_trgm_ops — and whether it is
	// depends on the search path of the connection that asked, not on the
	// index, so the qualifier is dropped: the class is compared by its bare
	// name, and where it lives is the emitter's business.
	if sp := strings.LastIndexByte(p, ' '); sp > 0 &&
		strings.HasSuffix(p, "_ops") && !strings.ContainsAny(p[sp:], "()") {
		c.OpClass = p[sp+1:]
		if dot := strings.LastIndexByte(c.OpClass, '.'); dot >= 0 {
			c.OpClass = c.OpClass[dot+1:]
		}
		p = strings.TrimSpace(p[:sp])
	}
	if i := strings.LastIndex(p, " COLLATE "); i >= 0 {
		c.Collate = unquote(strings.TrimSpace(p[i+len(" COLLATE "):]))
		p = strings.TrimSpace(p[:i])
	}
	if strings.ContainsAny(p, "()") {
		c.Name, c.Expr = unwrap(p), true
	} else {
		c.Name = unquote(p)
	}
	return c
}

// unquoteLit strips the single quotes pg_get_indexdef puts around a storage
// parameter value: fillfactor='70' is the number 70 to the model.
func unquoteLit(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// balanced returns the text inside the parenthesised group starting at open,
// and everything after it.
func balanced(s string, open int) (inner, after string) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return "", ""
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], s[i+1:]
			}
		}
	}
	return "", ""
}

// splitList splits on commas that are not inside parentheses or quotes.
func splitList(s string) []string {
	var out []string
	depth, start, inQuote := 0, 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		case ',':
			if depth == 0 && !inQuote {
				out = append(out, strings.TrimSpace(unquote(strings.TrimSpace(s[start:i]))))
				start = i + 1
			}
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, strings.TrimSpace(unquote(t)))
	}
	return out
}

// unwrap removes one layer of fully-enclosing parentheses.
func unwrap(s string) string {
	for len(s) > 1 && s[0] == '(' {
		inner, after := balanced(s, 0)
		if strings.TrimSpace(after) != "" {
			return s
		}
		s = strings.TrimSpace(inner)
	}
	return s
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
