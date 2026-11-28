// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package schemasqlite

import (
	"strings"
)

// Reading the CREATE statements SQLite keeps.
//
// The pragmas answer most questions — columns, keys, indexes, foreign keys —
// and none of them answers these: what a constraint is CALLED, what a CHECK
// says, what a generated column computes, whether a foreign key is deferrable,
// and what an index's expression key or partial predicate is. SQLite keeps
// only the statement it was given, verbatim, in sqlite_schema.sql, so those
// are read from there.
//
// Not a SQL parser. A CREATE TABLE body is a comma-separated list of column
// definitions and table constraints, and the only structure that matters for
// finding the commas is quoting and parentheses — which is what this tracks.

// splitTop splits s on sep where it is outside every quote and parenthesis.
func splitTop(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"', '`':
			i = skipQuoted(s, i, c)
		case '[':
			i = skipQuoted(s, i, ']')
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '*' {
				if j := strings.Index(s[i+2:], "*/"); j >= 0 {
					i += j + 3
				} else {
					i = len(s)
				}
			}
		case '(':
			depth++
		case ')':
			depth--
		default:
			if c == sep && depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// skipQuoted returns the index of the quote closing the one at i. A doubled
// quote inside is an escaped one, for every quoting style SQLite has.
func skipQuoted(s string, i int, close byte) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == close {
			if j+1 < len(s) && s[j+1] == close && close != ']' {
				j++
				continue
			}
			return j
		}
	}
	return len(s)
}

// matchParen returns the index of the parenthesis closing the one at i, or -1.
func matchParen(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch c := s[j]; c {
		case '\'', '"', '`':
			j = skipQuoted(s, j, c)
		case '[':
			j = skipQuoted(s, j, ']')
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// token is the next word, quoted name or punctuation of s at i, and the index
// after it. Whitespace before it is skipped.
func token(s string, i int) (string, int) {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return "", i
	}
	switch c := s[i]; c {
	case '"', '`', '\'':
		j := skipQuoted(s, i, c)
		if j >= len(s) {
			return s[i:], len(s)
		}
		return s[i : j+1], j + 1
	case '[':
		j := skipQuoted(s, i, ']')
		if j >= len(s) {
			return s[i:], len(s)
		}
		return s[i : j+1], j + 1
	case '(', ')', ',':
		return s[i : i+1], i + 1
	}
	j := i
	for j < len(s) && !isSpace(s[j]) && !strings.ContainsRune("(),\"'`[", rune(s[j])) {
		j++
	}
	return s[i:j], j
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// unquote turns a name as written into the name: "a""b" is a"b.
func unquote(s string) string {
	if len(s) >= 2 {
		switch {
		case s[0] == '"' && s[len(s)-1] == '"':
			return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
		case s[0] == '`' && s[len(s)-1] == '`':
			return strings.ReplaceAll(s[1:len(s)-1], "``", "`")
		case s[0] == '[' && s[len(s)-1] == ']':
			return s[1 : len(s)-1]
		case s[0] == '\'' && s[len(s)-1] == '\'':
			return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	}
	return s
}

// tableDef is what the CREATE TABLE statement says beyond the pragmas.
type tableDef struct {
	autoincrement bool
	generated     map[string]string // column → expression
	checks        []clause          // table and column CHECKs, in order
	uniques       []clause          // UNIQUE table constraints
	fks           []clause          // FOREIGN KEY table constraints
}

// parseTable reads a CREATE TABLE statement. A statement it cannot read — a
// CREATE TABLE … AS SELECT, a virtual table — yields an empty definition, and
// the pragmas alone describe the table.
func parseTable(sql string) tableDef {
	d := tableDef{generated: map[string]string{}}
	open := strings.IndexByte(sql, '(')
	if open < 0 {
		return d
	}
	close := matchParen(sql, open)
	if close < 0 {
		return d
	}
	if strings.Contains(strings.ToUpper(sql[:open]), " VIRTUAL ") {
		return d
	}
	for _, part := range splitTop(sql[open+1:close], ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		first, _ := token(part, 0)
		switch strings.ToUpper(first) {
		case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN":
			d.tableConstraint(part)
		default:
			d.column(part)
		}
	}
	d.autoincrement = strings.Contains(strings.ToUpper(sql), "AUTOINCREMENT")
	return d
}

// tableConstraint reads one table constraint: [CONSTRAINT name] then a
// PRIMARY KEY, UNIQUE, CHECK or FOREIGN KEY.
func (d *tableDef) tableConstraint(part string) {
	name := ""
	w, i := token(part, 0)
	if strings.EqualFold(w, "CONSTRAINT") {
		n, j := token(part, i)
		name = unquote(n)
		w, i = token(part, j)
	}
	switch strings.ToUpper(w) {
	case "UNIQUE":
		if cols, ok := parenList(part, i); ok {
			d.uniques = append(d.uniques, clause{name: name, cols: cols})
		}
	case "CHECK":
		if e, ok := parenBody(part, i); ok {
			d.checks = append(d.checks, clause{name: name, expr: e})
		}
	case "FOREIGN":
		_, j := token(part, i) // KEY
		cols, ok := parenList(part, j)
		if !ok {
			return
		}
		up := strings.ToUpper(part)
		d.fks = append(d.fks, clause{name: name, cols: cols,
			deferrable: strings.Contains(up, "DEFERRABLE") && !strings.Contains(up, "NOT DEFERRABLE") &&
				strings.Contains(up, "INITIALLY DEFERRED")})
	}
}

// column reads what a column definition says that its pragma row does not: a
// generated expression, and its CHECKs.
func (d *tableDef) column(part string) {
	n, i := token(part, 0)
	col := unquote(n)
	name := ""
	for i < len(part) {
		w, j := token(part, i)
		if w == "" {
			return
		}
		switch strings.ToUpper(w) {
		case "CONSTRAINT":
			nm, k := token(part, j)
			name = unquote(nm)
			i = k
			continue
		case "CHECK":
			e, ok := parenBody(part, j)
			if !ok {
				return
			}
			d.checks = append(d.checks, clause{name: name, column: col, expr: e})
			name = ""
			i = skipParen(part, j)
			continue
		case "AS":
			e, ok := parenBody(part, j)
			if !ok {
				return
			}
			d.generated[col] = e
			i = skipParen(part, j)
			continue
		case "DEFAULT":
			// Skip the default itself, which may be a parenthesised expression
			// holding words this loop would otherwise read as clauses.
			v, k := token(part, j)
			if v == "(" {
				k = skipParen(part, j)
			}
			i = k
			continue
		case "(":
			// A type's arguments, VARCHAR(40), or a REFERENCES column list.
			i = skipParen(part, j-1)
			continue
		}
		i = j
	}
}

// parenBody is the text inside the parentheses that open at or after i.
func parenBody(s string, i int) (string, bool) {
	open := strings.IndexByte(s[i:], '(')
	if open < 0 {
		return "", false
	}
	open += i
	close := matchParen(s, open)
	if close < 0 {
		return "", false
	}
	return strings.TrimSpace(s[open+1 : close]), true
}

// skipParen is the index after the parenthesised group opening at or after i.
func skipParen(s string, i int) int {
	open := strings.IndexByte(s[i:], '(')
	if open < 0 {
		return len(s)
	}
	close := matchParen(s, open+i)
	if close < 0 {
		return len(s)
	}
	return close + 1
}

// parenList is a parenthesised list of names.
func parenList(s string, i int) ([]string, bool) {
	body, ok := parenBody(s, i)
	if !ok {
		return nil, false
	}
	var out []string
	for _, p := range splitTop(body, ',') {
		w, _ := token(p, 0)
		out = append(out, unquote(w))
	}
	return out, true
}

// parseIndex reads CREATE [UNIQUE] INDEX name ON table (keys) [WHERE pred]:
// each key's text, which index_xinfo does not give for an expression, and the
// partial predicate.
func parseIndex(sql string) (keys []string, where string) {
	up := strings.ToUpper(sql)
	on := strings.Index(up, " ON ")
	if on < 0 {
		return nil, ""
	}
	open := strings.IndexByte(sql[on:], '(')
	if open < 0 {
		return nil, ""
	}
	open += on
	close := matchParen(sql, open)
	if close < 0 {
		return nil, ""
	}
	for _, k := range splitTop(sql[open+1:close], ',') {
		keys = append(keys, keyExpr(strings.TrimSpace(k)))
	}
	rest := strings.TrimSpace(sql[close+1:])
	if w, i := token(rest, 0); strings.EqualFold(w, "WHERE") {
		where = strings.TrimSpace(rest[i:])
	}
	return keys, where
}

// keyExpr strips an index key's trailing COLLATE and direction, and the one
// pair of parentheses compile/liteddl puts around an expression key.
func keyExpr(k string) string {
	for {
		up := strings.ToUpper(k)
		switch {
		case strings.HasSuffix(up, " DESC"):
			k = strings.TrimSpace(k[:len(k)-5])
			continue
		case strings.HasSuffix(up, " ASC"):
			k = strings.TrimSpace(k[:len(k)-4])
			continue
		}
		if i := lastTopWord(k, "COLLATE"); i >= 0 {
			k = strings.TrimSpace(k[:i])
			continue
		}
		break
	}
	if len(k) > 1 && k[0] == '(' && matchParen(k, 0) == len(k)-1 {
		k = strings.TrimSpace(k[1 : len(k)-1])
	}
	return k
}

// lastTopWord is where the word w begins as the second-to-last top-level
// token of k — `expr COLLATE name` — or -1.
func lastTopWord(k, w string) int {
	parts := splitTop(k, ' ')
	if len(parts) < 3 {
		return -1
	}
	if !strings.EqualFold(strings.TrimSpace(parts[len(parts)-2]), w) {
		return -1
	}
	return len(strings.Join(parts[:len(parts)-2], " "))
}
