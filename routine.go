package storm

import "github.com/gsoultan/storm/schema"

// Declaring the SQL-bodied objects: functions, views and triggers.
//
// These are the one part of a model that storm does not build from Go. A
// column comes from a field and an index from a field pointer, so the editor
// checks both and a rename reaches them; a PL/pgSQL body is a language storm
// does not parse, and the honest thing is to say so rather than to invent a
// DSL that covers a third of it.
//
// What storm does instead is own the LIFECYCLE. The body is text, but when it
// changes, whether it is still valid against the tables it reads, what order
// it has to be created in, and what has to be dropped before it can be
// replaced — those are storm's, and they are the parts that get a schema wrong.
// A declaration here is checked the way every other declaration is: applied to
// a scratch schema at generation time, so a body that does not compile fails
// the build rather than the first request.
//
// Pass the results to Build alongside the models, the way a union is passed:
//
//	storm.Build(&Role{}, &Grant{},
//	    storm.Function("authorize", ...),
//	    storm.Trigger("grants_bump", "grants", ...))

// FunctionDecl is a declared function. Pass it to Build alongside the models.
type FunctionDecl struct{ f *schema.Function }

// Function declares a stored function.
//
// Args and returns are written as PostgreSQL would accept them —
// "p_tenant uuid, p_subject uuid" and "boolean". Body is the text between the
// dollar quotes; storm picks a quote tag that does not collide with it.
func Function(name, args, returns string) *FunctionDecl {
	return &FunctionDecl{f: &schema.Function{
		Name: name, Args: args, Returns: returns, Language: "plpgsql",
	}}
}

// Name reports the declared name; the generate command uses it.
func (d *FunctionDecl) Name() string { return d.f.Name }

// Language sets the body's language. The default is plpgsql.
//
// The choice is not cosmetic: PostgreSQL PARSES a `sql` body when the function
// is created, so it fails at generation time if it reads a column that is not
// there — which is what you want — while a plpgsql body is only syntax-checked
// and its table references are resolved on first execution.
func (d *FunctionDecl) Language(l string) *FunctionDecl { d.f.Language = l; return d }

// Stable marks the function STABLE: it cannot modify the database and returns
// the same answer for the same arguments within one statement. Required for a
// function to be used in an index or pushed into a parallel plan.
func (d *FunctionDecl) Stable() *FunctionDecl { d.f.Volatility = "STABLE"; return d }

// Immutable marks the function IMMUTABLE: same answer forever, for the same
// arguments. PostgreSQL is entitled to fold it to a constant at plan time, so
// a function that reads a table must NOT be marked this way.
func (d *FunctionDecl) Immutable() *FunctionDecl { d.f.Volatility = "IMMUTABLE"; return d }

// Strict makes the function return NULL whenever any argument is NULL, without
// executing the body.
func (d *FunctionDecl) Strict() *FunctionDecl { d.f.Strict = true; return d }

// SecurityDefiner runs the function as its owner rather than its caller.
//
// It is how a function reaches a table the caller cannot, and for the same
// reason it is how a caller reaches a table they should not: the body runs with
// the owner's rights, so anything it interpolates, any search_path it trusts,
// and any function it calls is a privilege boundary. Set search_path explicitly
// in a body that uses this.
func (d *FunctionDecl) SecurityDefiner() *FunctionDecl { d.f.Security = "DEFINER"; return d }

// Body sets the function body.
func (d *FunctionDecl) Body(sql string) *FunctionDecl { d.f.Body = sql; return d }

// ViewDecl is a declared view. Pass it to Build alongside the models.
type ViewDecl struct{ v *schema.View }

// View declares a view over the given SELECT, written without CREATE VIEW.
func View(name, query string) *ViewDecl {
	return &ViewDecl{v: &schema.View{Name: name, Query: query}}
}

// Name reports the declared name; the generate command uses it.
func (d *ViewDecl) Name() string { return d.v.Name }

// TriggerDecl is a declared trigger. Pass it to Build alongside the models.
type TriggerDecl struct{ t *schema.Trigger }

// Trigger declares a trigger on a table. The statement is the whole
// CREATE TRIGGER, because every part of one — timing, events, transition
// tables, WHEN, FOR EACH — is a PostgreSQL grammar storm would otherwise have
// to mirror clause for clause to say nothing new.
//
// Prefer FOR EACH STATEMENT with transition tables (REFERENCING NEW TABLE AS
// ...) over FOR EACH ROW on any table that takes bulk writes: a row trigger
// fires once per row and turns one COPY into a million function calls.
func Trigger(name, table, statement string) *TriggerDecl {
	return &TriggerDecl{t: &schema.Trigger{Name: name, Table: table, Create: statement}}
}

// Name reports the declared name; the generate command uses it.
func (d *TriggerDecl) Name() string { return d.t.Name }
