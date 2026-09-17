package migrate

import (
	"strings"

	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/schema"
)

// Diffing the SQL-bodied objects.
//
// Both sides reaching here have been through Normalize, so a body is compared
// as PostgreSQL stores it, not as somebody typed it (schema/routine.go).

// addRoutines appends the creates, in dependency order: a view may call a
// function and a trigger executes one, so functions land first.
func addRoutines(p *Plan, from, to *schema.Schema) {
	// Functions are keyed by SIGNATURE, not name. PostgreSQL overloads on the
	// argument list, so `authorize(uuid, text)` and `authorize(uuid, jsonb)`
	// are two functions that happen to share a name — keying by name would
	// make adding an overload look like editing the original, and the plan
	// would REPLACE the one that was already right.
	old := map[string]*schema.Function{}
	for _, f := range from.Functions {
		old[f.Signature()] = f
	}
	for _, f := range to.Functions {
		prev, existed := old[f.Signature()]
		switch {
		case !existed:
			p.add(Change{SQL: pgddl.CreateFunction(f)})
		case prev.SameAs(f):
			// unchanged
		case prev.Returns != f.Returns:
			// CREATE OR REPLACE cannot change a return type — PostgreSQL
			// refuses with "cannot change return type of existing function" —
			// so the old one has to go first. Destructive because the DROP
			// fails outright if a view or another function depends on it, and
			// that failure is the point: the dependents need looking at.
			p.add(Change{
				SQL:         pgddl.DropFunction(prev),
				Destructive: true,
				Why: "function " + f.Signature() + " changed its return type from " +
					prev.Returns + " to " + f.Returns + "; anything depending on it must be dropped too",
			})
			p.add(Change{SQL: pgddl.CreateFunction(f)})
		default:
			p.add(Change{SQL: pgddl.CreateFunction(f)})
		}
	}

	for _, v := range to.Views {
		prev := from.View(v.Name)
		if prev == nil {
			p.add(Change{SQL: pgddl.CreateView(v)})
			continue
		}
		if prev.Def == v.Def {
			continue
		}
		// CREATE OR REPLACE VIEW only works when the new definition keeps the
		// old columns, same names, same types, same order, and may only ADD to
		// the end. Anything else — a dropped column, a renamed one, a widened
		// type — needs a DROP first, which takes every dependent view with it.
		if !columnsPreserved(prev.Def, v.Def) {
			p.add(Change{
				SQL:         pgddl.DropView(v.Name),
				Destructive: true,
				Why: "view " + v.Name + " changed a column, so CREATE OR REPLACE cannot " +
					"be used; anything selecting from it is dropped with it",
			})
		}
		p.add(Change{SQL: pgddl.CreateView(v)})
	}

	// A trigger is replaced by dropping and recreating it, never by
	// CREATE OR REPLACE TRIGGER: that form inherits the existing trigger's
	// enabled state, so a trigger somebody had disabled comes back disabled
	// and the migration reports success while the trigger fires for nobody.
	for _, t := range to.Triggers {
		prev := from.Trigger(t.Name)
		if prev != nil && prev.Table == t.Table {
			if prev.Def == t.Def {
				continue
			}
			p.add(Change{SQL: pgddl.DropTrigger(t.Name, t.Table)})
		}
		p.add(Change{SQL: pgddl.CreateTrigger(t)})
	}
}

// dropRoutines appends the drops in reverse dependency order — triggers, then
// views, then functions — so nothing is dropped while something still refers to
// it. It runs BEFORE tables are dropped: a view over a dropped table has to go
// first, whereas a trigger ON a dropped table would have gone with it anyway.
func dropRoutines(p *Plan, from, to *schema.Schema) {
	for _, t := range from.Triggers {
		if cur := to.Trigger(t.Name); cur == nil || cur.Table != t.Table {
			p.add(Change{
				SQL:         pgddl.DropTrigger(t.Name, t.Table),
				Destructive: true,
				Why:         "trigger " + t.Name + " on " + t.Table + " is no longer in the model",
			})
		}
	}
	for _, v := range from.Views {
		if to.View(v.Name) == nil {
			p.add(Change{
				SQL:         pgddl.DropView(v.Name),
				Destructive: true,
				Why:         "view " + v.Name + " is no longer in the model",
			})
		}
	}
	want := map[string]bool{}
	for _, f := range to.Functions {
		want[f.Signature()] = true
	}
	for _, f := range from.Functions {
		if !want[f.Signature()] {
			p.add(Change{
				SQL:         pgddl.DropFunction(f),
				Destructive: true,
				Why:         "function " + f.Signature() + " is no longer in the model",
			})
		}
	}
}

// columnsPreserved reports whether two view definitions agree on their leading
// columns, which is the condition CREATE OR REPLACE VIEW imposes.
//
// It is deliberately conservative: it compares the SELECT list textually, and
// anything it cannot read as a plain list it calls changed. A false "changed"
// costs a DROP and CREATE that the adopter sees in a reviewed migration; a
// false "preserved" ships a plan that fails when applied.
func columnsPreserved(oldDef, newDef string) bool {
	o, ok1 := selectList(oldDef)
	n, ok2 := selectList(newDef)
	if !ok1 || !ok2 || len(n) < len(o) {
		return false
	}
	for i := range o {
		if o[i] != n[i] {
			return false
		}
	}
	return true
}

// selectList returns the output column names of a view definition, and whether
// it could read them at all.
//
// It scans rather than splits because a view body is full of characters that
// mean nothing where they appear: commas inside a function call, the word FROM
// inside a subquery, parentheses inside a string literal. Depth and quote
// tracking is the minimum that gets those right, and anything past that —
// a set-returning function in the target list, a UNION — returns false and is
// treated as changed.
func selectList(def string) ([]string, bool) {
	s := strings.TrimSpace(def)
	up := strings.ToUpper(s)
	if !strings.HasPrefix(up, "SELECT ") {
		return nil, false
	}
	s = s[len("SELECT "):]

	var (
		items   []string
		cur     strings.Builder
		depth   int
		inQuote byte
	)
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			items = append(items, outputName(t))
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			cur.WriteByte(c)
			if c == inQuote {
				// '' and "" are escapes for the quote character itself.
				if i+1 < len(s) && s[i+1] == inQuote {
					cur.WriteByte(s[i+1])
					i++
					continue
				}
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			inQuote = c
			cur.WriteByte(c)
		case '(':
			depth++
			cur.WriteByte(c)
		case ')':
			depth--
			cur.WriteByte(c)
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			cur.WriteByte(c)
		default:
			if depth == 0 && isWordBoundary(s, i, "FROM") {
				flush()
				return items, len(items) > 0
			}
			cur.WriteByte(c)
		}
	}
	// A view with no FROM at all — SELECT 1 AS x — is legal.
	flush()
	return items, len(items) > 0
}

// isWordBoundary reports whether word starts at i and stands alone there.
func isWordBoundary(s string, i int, word string) bool {
	if i+len(word) > len(s) || !strings.EqualFold(s[i:i+len(word)], word) {
		return false
	}
	if i > 0 && isIdentByte(s[i-1]) {
		return false
	}
	return i+len(word) == len(s) || !isIdentByte(s[i+len(word)])
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// outputName is the column name PostgreSQL gives one select item: the alias if
// there is one, otherwise the trailing identifier of a column reference.
func outputName(item string) string {
	if i := lastTopLevel(item, " AS "); i >= 0 {
		return strings.Trim(strings.TrimSpace(item[i+4:]), `"`)
	}
	f := strings.Fields(item)
	last := f[len(f)-1]
	if j := strings.LastIndexByte(last, '.'); j >= 0 {
		last = last[j+1:]
	}
	return strings.Trim(last, `"`)
}

// lastTopLevel finds sep outside parens and quotes, searching from the end.
func lastTopLevel(s, sep string) int {
	depth, inQuote, found := 0, byte(0), -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			inQuote = c
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth == 0 && i+len(sep) <= len(s) && strings.EqualFold(s[i:i+len(sep)], sep) {
				found = i
			}
		}
	}
	return found
}
