// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgexpr

import "strings"

// token is one lexeme: an identifier (lowercased unless it was quoted), a
// number, a string literal's value, or an operator.
type token struct {
	kind byte // 'i' ident, 'q' quoted ident, 'n' number, 's' string, 'o' operator
	text string
}

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for {
				if j >= len(s) {
					return nil, fail("an unterminated string literal")
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			out = append(out, token{'s', b.String()})
			i = j + 1
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return nil, fail("an unterminated quoted identifier")
			}
			out = append(out, token{'q', s[i+1 : i+1+j]})
			i += j + 2
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j, dot := i, false
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' && !dot) {
				if s[j] == '.' {
					dot = true
				}
				j++
			}
			out = append(out, token{'n', s[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, token{'i', strings.ToLower(s[i:j])})
			i = j
		default:
			op := ""
			for _, o := range []string{"<>", "!=", "<=", ">=", "::", "=", "<", ">", "(", ")", ",", "+", "-", "*", "/", "%"} {
				if strings.HasPrefix(s[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, fail("the character " + strconvQuote(s[i:i+1]))
			}
			out = append(out, token{'o', op})
			i += len(op)
		}
	}
	return out, nil
}

func strconvQuote(s string) string { return "'" + s + "'" }
