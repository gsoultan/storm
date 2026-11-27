// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgddl

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// The SQL-bodied objects. Each renders from what the adopter declared; the
// diff never reads these, it reads the catalog form normalisation produced —
// see schema/routine.go for why the two are kept apart.

// CreateFunction renders CREATE OR REPLACE FUNCTION.
//
// OR REPLACE rather than plain CREATE because that is what makes a function
// change non-destructive: the body is replaced in place, no dependent view or
// trigger is invalidated, and re-running a migration is a no-op. It is also
// the only form that works when the function already exists with the same
// signature, which on a re-applied plan it does.
func CreateFunction(f *schema.Function) string {
	var b strings.Builder
	b.WriteString("CREATE OR REPLACE FUNCTION " + Ident(f.Name) + "(" + f.Args + ")\n")
	b.WriteString(" RETURNS " + f.Returns + "\n")
	b.WriteString(" LANGUAGE " + f.Language)
	if v := strings.ToUpper(f.Volatility); v != "" && v != "VOLATILE" {
		b.WriteString("\n " + v)
	}
	if f.Strict {
		b.WriteString("\n STRICT")
	}
	if strings.EqualFold(f.Security, "DEFINER") {
		b.WriteString("\n SECURITY DEFINER")
	}
	tag := dollarTag(f.Body)
	b.WriteString("\nAS " + tag + f.Body + tag + ";")
	return b.String()
}

// DropFunction names the argument types as well as the name: PostgreSQL allows
// overloads, so DROP FUNCTION f is ambiguous the moment a second f exists.
func DropFunction(f *schema.Function) string {
	return "DROP FUNCTION " + Ident(f.Name) + "(" + f.Args + ");"
}

// CreateView renders CREATE OR REPLACE VIEW.
//
// OR REPLACE is weaker here than it is for a function — PostgreSQL only
// accepts it when the new definition keeps the same column names and types in
// the same order, and refuses it otherwise. The diff handles that refusal by
// dropping and recreating, which is why a column removed from a view is marked
// destructive: anything depending on the view goes with it.
func CreateView(v *schema.View) string {
	q := strings.TrimRight(strings.TrimSpace(v.SQL()), ";")
	return "CREATE OR REPLACE VIEW " + Ident(v.Name) + " AS\n" + q + ";"
}

func DropView(name string) string { return "DROP VIEW " + Ident(name) + ";" }

// CreateTrigger renders the trigger's own CREATE statement.
//
// There is no OR REPLACE for a trigger before PostgreSQL 14 and storm does not
// use the one there is: CREATE OR REPLACE TRIGGER silently keeps the old
// trigger's enabled/disabled state, so a replaced trigger can come back
// disabled and fire for nobody. DROP then CREATE is two statements in one
// transaction and has no such memory.
func CreateTrigger(t *schema.Trigger) string {
	return strings.TrimRight(strings.TrimSpace(t.SQL()), ";") + ";"
}

// DropTrigger uses IF EXISTS because it is paired with CreateTrigger on the
// replace path, where the trigger may legitimately not be there yet.
func DropTrigger(name, table string) string {
	return "DROP TRIGGER IF EXISTS " + Ident(name) + " ON " + Ident(table) + ";"
}

// dollarTag picks a dollar-quote tag that does not occur in the body.
//
// A body is arbitrary text that routinely contains $$ of its own — every
// PL/pgSQL function storm will ever be handed was itself written between
// dollar quotes — so a fixed tag is a syntax error waiting for the first
// nested function. Trying successive tags terminates because each candidate is
// longer than the last and the body is finite.
func dollarTag(body string) string {
	for i := 0; ; i++ {
		tag := "$storm$"
		if i > 0 {
			tag = "$storm" + strconv.Itoa(i) + "$"
		}
		if !strings.Contains(body, tag) {
			return tag
		}
	}
}

// ViewOrder is views in an order PostgreSQL can create them in: each after
// every other view its definition reads, and otherwise as given. A view over a
// view that sorts after it by name is the ordinary case — a summary over a
// detail view — and creating them in name order fails on the first.
//
// "Reads" is judged by the other view's name appearing in the definition as a
// whole identifier, which can only err towards waiting for a view that was not
// needed. A cycle, which PostgreSQL could not create either, keeps the order
// it was given.
func ViewOrder(views []*schema.View) []*schema.View {
	out := make([]*schema.View, 0, len(views))
	placed := map[string]bool{}
	for len(out) < len(views) {
		progress := false
		for _, v := range views {
			if placed[v.Name] {
				continue
			}
			ready := true
			for _, w := range views {
				if w.Name != v.Name && !placed[w.Name] && mentionsIdent(v.SQL(), w.Name) {
					ready = false
					break
				}
			}
			if ready {
				out, placed[v.Name], progress = append(out, v), true, true
			}
		}
		if !progress {
			for _, v := range views {
				if !placed[v.Name] {
					out, placed[v.Name] = append(out, v), true
				}
			}
		}
	}
	return out
}

// mentionsIdent reports whether name appears in sql as a whole identifier,
// quoted or not.
func mentionsIdent(sql, name string) bool {
	low, n := strings.ToLower(sql), strings.ToLower(name)
	ident := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 0x80
	}
	for i := 0; ; {
		j := strings.Index(low[i:], n)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(n)
		if (j == 0 || !ident(low[j-1])) && (end == len(low) || !ident(low[end])) {
			return true
		}
		i = j + 1
	}
}
