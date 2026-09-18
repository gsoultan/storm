package pgddl_test

// The SQL-bodied objects: functions, views and triggers.
//
// Each of these renders a statement whose FORM carries a decision — OR REPLACE
// or not, a dollar tag chosen against the body, argument types in a DROP — and
// getting any of them wrong is a migration that fails on the adopter's server
// rather than here.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/schema"
)

func TestCreateFunctionReplacesInPlace(t *testing.T) {
	f := &schema.Function{
		Name: "authorize", Args: "p_tenant uuid, p_subject uuid",
		Returns: "boolean", Language: "plpgsql", Body: "\nBEGIN\n  RETURN true;\nEND\n",
	}
	got := pgddl.CreateFunction(f)

	// OR REPLACE is what makes a function change non-destructive: the body is
	// replaced in place, no dependent view or trigger is invalidated, and
	// re-running a migration is a no-op. Plain CREATE fails the second time.
	if !strings.HasPrefix(got, `CREATE OR REPLACE FUNCTION "authorize"(p_tenant uuid, p_subject uuid)`) {
		t.Errorf("not a replacing create:\n%s", got)
	}
	for _, want := range []string{" RETURNS boolean", " LANGUAGE plpgsql", "$storm$"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// VOLATILE is PostgreSQL's default, so saying it adds noise to every
	// migration and changes nothing.
	if strings.Contains(got, "VOLATILE") {
		t.Errorf("the default volatility was spelled out:\n%s", got)
	}
	if strings.Contains(got, "STRICT") || strings.Contains(got, "SECURITY DEFINER") {
		t.Errorf("an unset attribute was emitted:\n%s", got)
	}
	if !strings.HasSuffix(got, ";") {
		t.Errorf("unterminated:\n%s", got)
	}
}

func TestCreateFunctionCarriesItsAttributes(t *testing.T) {
	f := &schema.Function{
		Name: "slug", Args: "t text", Returns: "text", Language: "sql",
		Volatility: "immutable", Strict: true, Security: "definer",
		Body: " SELECT lower($1) ",
	}
	got := pgddl.CreateFunction(f)
	for _, want := range []string{"IMMUTABLE", "STRICT", "SECURITY DEFINER"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Case-insensitively declared, upper-cased in the DDL: the catalog reports
	// them upper and a diff compares text.
	if strings.Contains(got, "immutable") || strings.Contains(got, "definer") {
		t.Errorf("an attribute was emitted in the case it was declared:\n%s", got)
	}
}

// A body is arbitrary text that routinely contains $$ of its own — every
// PL/pgSQL function storm will be handed was itself written between dollar
// quotes — so a fixed tag is a syntax error waiting for the first nested one.
func TestDollarTagAvoidsTheBody(t *testing.T) {
	plain := pgddl.CreateFunction(&schema.Function{
		Name: "f", Returns: "void", Language: "plpgsql", Body: "BEGIN END",
	})
	if !strings.Contains(plain, "$storm$BEGIN END$storm$") {
		t.Errorf("the default tag was not used:\n%s", plain)
	}

	// A body that already contains the default tag must get a different one,
	// or the quote closes early and the rest of the body becomes SQL.
	nested := pgddl.CreateFunction(&schema.Function{
		Name: "f", Returns: "void", Language: "plpgsql",
		Body: "BEGIN EXECUTE $storm$SELECT 1$storm$; END",
	})
	if strings.Contains(nested, "$storm$BEGIN") {
		t.Errorf("the tag collides with the body:\n%s", nested)
	}
	if !strings.Contains(nested, "$storm1$BEGIN") {
		t.Errorf("no distinct tag was chosen:\n%s", nested)
	}
	// ...and one that contains BOTH keeps going.
	both := pgddl.CreateFunction(&schema.Function{
		Name: "f", Returns: "void", Language: "plpgsql",
		Body: "$storm$ $storm1$",
	})
	if !strings.Contains(both, "$storm2$") {
		t.Errorf("the search stopped at the second candidate:\n%s", both)
	}
}

// PostgreSQL allows overloads, so DROP FUNCTION f is ambiguous the moment a
// second f exists — and the migration would drop the wrong one or refuse.
func TestDropFunctionNamesItsArgumentTypes(t *testing.T) {
	got := pgddl.DropFunction(&schema.Function{
		Name: "authorize", Args: "p_tenant uuid, p_subject uuid",
	})
	want := `DROP FUNCTION "authorize"(p_tenant uuid, p_subject uuid);`
	if got != want {
		t.Errorf("DropFunction = %q, want %q", got, want)
	}
}

func TestViewsCreateAndDrop(t *testing.T) {
	got := pgddl.CreateView(&schema.View{Name: "active_users", Query: "  SELECT * FROM users;  "})
	if !strings.HasPrefix(got, `CREATE OR REPLACE VIEW "active_users" AS`) {
		t.Errorf("not a replacing create:\n%s", got)
	}
	// One terminator, not two: the declared query may or may not carry its
	// own, and `...;;` is a syntax error.
	if strings.Contains(got, ";;") {
		t.Errorf("the declared semicolon was kept as well as added:\n%s", got)
	}
	if !strings.HasSuffix(got, "SELECT * FROM users;") {
		t.Errorf("the query did not survive trimming:\n%s", got)
	}

	if got := pgddl.DropView("active_users"); got != `DROP VIEW "active_users";` {
		t.Errorf("DropView = %q", got)
	}
}

// storm does NOT use CREATE OR REPLACE TRIGGER even where PostgreSQL has it:
// it silently keeps the old trigger's enabled/disabled state, so a replaced
// trigger can come back disabled and fire for nobody.
func TestTriggersDropAndCreateRatherThanReplace(t *testing.T) {
	got := pgddl.CreateTrigger(&schema.Trigger{
		Name: "touch", Table: "users",
		Create: "CREATE TRIGGER touch BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION f()  ",
	})
	if strings.Contains(strings.ToUpper(got), "OR REPLACE") {
		t.Errorf("a trigger was replaced rather than recreated:\n%s", got)
	}
	if !strings.HasSuffix(got, "EXECUTE FUNCTION f();") {
		t.Errorf("the declared statement was not terminated exactly once:\n%s", got)
	}

	// IF EXISTS because the drop is paired with the create on the replace
	// path, where the trigger may legitimately not be there yet.
	drop := pgddl.DropTrigger("touch", "users")
	if drop != `DROP TRIGGER IF EXISTS "touch" ON "users";` {
		t.Errorf("DropTrigger = %q", drop)
	}
}
