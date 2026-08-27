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

// parseIndexDef reads a CREATE INDEX statement back into key columns and an
// optional partial predicate.
func parseIndexDef(def string) ([]schema.IndexColumn, string) {
	open := strings.Index(def, " USING ")
	if open < 0 {
		return nil, ""
	}
	open = strings.Index(def[open:], "(")
	if open < 0 {
		return nil, ""
	}
	open += strings.Index(def, " USING ")
	body, after := balanced(def, open)

	var cols []schema.IndexColumn
	for _, part := range splitList(body) {
		c := schema.IndexColumn{}
		p := strings.TrimSpace(part)
		if strings.HasSuffix(p, " NULLS LAST") {
			c.NullsLast = true
			p = strings.TrimSpace(strings.TrimSuffix(p, " NULLS LAST"))
		} else if strings.HasSuffix(p, " NULLS FIRST") {
			p = strings.TrimSpace(strings.TrimSuffix(p, " NULLS FIRST"))
		}
		if strings.HasSuffix(p, " DESC") {
			c.Desc = true
			p = strings.TrimSpace(strings.TrimSuffix(p, " DESC"))
		} else if strings.HasSuffix(p, " ASC") {
			p = strings.TrimSpace(strings.TrimSuffix(p, " ASC"))
		}
		// Trailing operator classes (text_pattern_ops, etc).
		if sp := strings.LastIndexByte(p, ' '); sp > 0 && strings.HasSuffix(p, "_ops") {
			p = strings.TrimSpace(p[:sp])
		}
		if strings.ContainsAny(p, "()") {
			c.Name, c.Expr = unwrap(p), true
		} else {
			c.Name = unquote(p)
		}
		cols = append(cols, c)
	}

	where := ""
	if k := strings.Index(after, "WHERE "); k >= 0 {
		where = unwrap(strings.TrimSpace(after[k+len("WHERE "):]))
	}
	return cols, where
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
