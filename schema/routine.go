package schema

// The SQL-bodied schema objects: functions, views and triggers.
//
// Every other object in the IR is structural — storm knows what a column or an
// index MEANS, so it can build one from field pointers and reason about a
// change to it. These three carry a body storm does not parse, and pretending
// otherwise would mean shipping a PL/pgSQL front end nobody asked for.
//
// So storm treats the body the way it already treats a CHECK expression
// (migrate/normalize.go): it does not canonicalise the text itself, it runs the
// declaration through PostgreSQL and compares catalog form against catalog
// form. That is why each type below separates what the ADOPTER wrote from what
// the CATALOG returned — Normalize fills the second from the first, and the
// diff only ever reads the second.
//
// The alternative, comparing adopter text to catalog text, does not work for
// the same reason it does not work for expressions: PostgreSQL rewrites what it
// stores. A view's `SELECT *` comes back with every column named.

// Function is a stored function.
//
// The fields are the catalog's own decomposition rather than one definition
// string, because pg_get_functiondef() schema-qualifies the name — and the two
// sides of a diff live in DIFFERENT namespaces by construction, the model in a
// scratch schema and the database in its real one. Comparing definition text
// would report every function as changed, every time.
//
// Body is exempt from that worry: PostgreSQL stores a function body verbatim
// and hands it back unrewritten, so it is the one piece of SQL text in the IR
// that compares exactly.
type Function struct {
	Name string

	// Args and Returns are catalog form — "p_tenant uuid, p_subject uuid" and
	// "boolean" — as pg_get_function_arguments and pg_get_function_result
	// render them. An adopter may write them in any form PostgreSQL accepts;
	// normalisation replaces both with what the catalog says.
	Args    string
	Returns string

	Language string // "sql", "plpgsql"

	// Volatility is VOLATILE, STABLE or IMMUTABLE. Empty means VOLATILE,
	// PostgreSQL's default, and normalisation makes it explicit.
	Volatility string

	Strict   bool
	Security string // "INVOKER" (default) or "DEFINER"

	// Body is the text between the dollar quotes, verbatim.
	//
	// Empty for a function declared with a SQL-standard BEGIN ATOMIC body:
	// PostgreSQL parses that form into a stored parse tree and prosrc is null,
	// so there is nothing verbatim to compare. Def carries it instead.
	Body string

	// Def is the whole catalog definition, set by introspection only. It is
	// the fallback comparison key for the BEGIN ATOMIC form, and it is not
	// namespace-stable, so the diff uses it only when Body is empty on both
	// sides.
	Def string
}

// View is a view.
//
// Def is what pg_get_viewdef returns. Unlike a function definition it carries
// no schema qualification of its OWN name, and the table references inside it
// are qualified relative to search_path — which introspection sets to the
// namespace it is reading (schema/pg.setSearchPath). Both sides of a diff are
// therefore read with the view's own schema in the path, and both come back
// unqualified.
type View struct {
	Name string

	// Query is what the adopter declared: the SELECT, without CREATE VIEW.
	// Normalisation replaces Def with the catalog's rewrite of it.
	Query string

	Def string
}

// Trigger is a trigger.
//
// A trigger has more shape than it looks: timing, an event mask, a row/statement
// choice, transition tables, a WHEN clause, and UPDATE OF column lists. storm
// keeps the catalog's rendered definition rather than decomposing all of it,
// because pg_get_triggerdef is exact, total, and stable across versions — and
// re-deriving it from the tgtype bitmask is a bug farm that buys nothing: no
// part of storm reasons about a trigger's internals, it only needs to know
// whether this trigger still matches that one.
type Trigger struct {
	Name string

	// Table is the relation the trigger fires on, unqualified.
	Table string

	// Create is what the adopter declared: the whole CREATE TRIGGER statement.
	Create string

	// Def is pg_get_triggerdef with the namespace stripped from the table
	// reference — the one qualification it does emit. Set by introspection.
	Def string
}

// ---- lookup helpers ----

func (s *Schema) Function(name string) *Function {
	for _, f := range s.Functions {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func (s *Schema) View(name string) *View {
	for _, v := range s.Views {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func (s *Schema) Trigger(name string) *Trigger {
	for _, t := range s.Triggers {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// SameAs reports whether two functions are the same declaration.
//
// Body is compared verbatim. When BOTH bodies are empty the function came from
// a BEGIN ATOMIC declaration on both sides and Def is all there is; when only
// one is empty the two forms genuinely differ and the function has changed.
func (f *Function) SameAs(o *Function) bool {
	if f.Args != o.Args || f.Returns != o.Returns ||
		f.Language != o.Language || f.Volatility != o.Volatility ||
		f.Strict != o.Strict || f.Security != o.Security {
		return false
	}
	if f.Body == "" && o.Body == "" {
		return f.Def == o.Def
	}
	return f.Body == o.Body
}

// Signature is the name and argument types, which is what identifies a function
// to DROP — PostgreSQL allows overloads, so the name alone does not.
func (f *Function) Signature() string { return f.Name + "(" + f.Args + ")" }

// SQL returns what to render: what the adopter declared, or — for a schema that
// came from introspection, where there is no adopter text — the catalog's.
func (v *View) SQL() string {
	if v.Query != "" {
		return v.Query
	}
	return v.Def
}

func (t *Trigger) SQL() string {
	if t.Create != "" {
		return t.Create
	}
	return t.Def
}
