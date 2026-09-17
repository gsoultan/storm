package migrate_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
)

// The gate this file holds: a schema containing functions, views and triggers
// must survive model → DDL → catalog → IR unchanged. If it does not, every
// `storm verify` reports drift that no migration can clear, because the plan
// it emits re-applies what is already there.

// withRoutines is a schema whose SQL-bodied objects between them use the
// features that break naive comparison: a body containing its own dollar
// quotes, a statement trigger with transition tables, a view over a filtered
// table, and a STABLE function reading a table.
func withRoutines() *schema.Schema {
	return &schema.Schema{
		Tables: []*schema.Table{{
			Name: "account",
			Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: "uuid"}, NotNull: true},
				{Name: "email", Type: schema.Type{Name: "text"}, NotNull: true},
				{Name: "active", Type: schema.Type{Name: "boolean"}, NotNull: true, Default: "true"},
			},
			PrimaryKey: []string{"id"},
		}, {
			Name: "audit",
			Columns: []*schema.Column{
				{Name: "id", Type: schema.Type{Name: "bigint"}, NotNull: true, Identity: true},
				{Name: "n", Type: schema.Type{Name: "bigint"}, NotNull: true},
			},
			PrimaryKey: []string{"id"},
		}},
		Functions: []*schema.Function{{
			Name:       "account_email",
			Args:       "p_id uuid",
			Returns:    "text",
			Language:   "sql",
			Volatility: "STABLE",
			Body:       "\n  SELECT email FROM account WHERE id = p_id;\n",
		}, {
			// A plpgsql body that itself contains $$ — the case a fixed
			// dollar-quote tag turns into a syntax error.
			Name:     "note_change",
			Args:     "",
			Returns:  "trigger",
			Language: "plpgsql",
			Body: "\nBEGIN\n" +
				"  -- a nested body, quoted the way anybody would write one: $$\n" +
				"  INSERT INTO audit(n) SELECT count(*) FROM newtab;\n" +
				"  RETURN NULL;\n" +
				"END;\n",
		}},
		Views: []*schema.View{{
			Name:  "active_account",
			Query: "SELECT id, email FROM account WHERE active",
		}},
		Triggers: []*schema.Trigger{{
			Name:  "account_ins",
			Table: "account",
			Create: "CREATE TRIGGER account_ins AFTER INSERT ON account " +
				"REFERENCING NEW TABLE AS newtab FOR EACH STATEMENT " +
				"EXECUTE FUNCTION note_change()",
		}},
	}
}

func TestRoutinesRoundTripToAnEmptyDiff(t *testing.T) {
	ctx, c := conn(t, "storm_rt_routines")
	want := withRoutines()

	if _, err := c.Exec(ctx, "CREATE SCHEMA storm_rt_routines"); err != nil {
		t.Fatal(err)
	}

	// Apply the model for real, then ask for the plan that would take the live
	// schema to the model. Everything is already there, so the only correct
	// answer is nothing.
	plan, err := migrate.ForWith(ctx, c, "storm_rt_routines", want, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("first plan was empty: the model was never applied, so this proves nothing")
	}
	if _, err := c.Exec(ctx, "SET search_path TO storm_rt_routines"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatalf("applying the plan failed: %v\n---\n%s", err, plan.SQL())
	}

	again, err := migrate.ForWith(ctx, c, "storm_rt_routines", want, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("model does not round-trip; storm would migrate forever:\n%s", again.SQL())
	}
}

// A changed body has to be noticed. The failure this guards against is the
// opposite of the one above, and a comparison that returns "same" for
// everything passes the round-trip test perfectly.
func TestRoutinesDiffSeesAChangedBody(t *testing.T) {
	ctx, c := conn(t, "storm_rt_changed")
	if _, err := c.Exec(ctx, "CREATE SCHEMA storm_rt_changed"); err != nil {
		t.Fatal(err)
	}
	base := withRoutines()
	plan, err := migrate.ForWith(ctx, c, "storm_rt_changed", base, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO storm_rt_changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*schema.Schema)
		expect string
	}{
		{"function body", func(s *schema.Schema) {
			s.Functions[0].Body = "\n  SELECT lower(email) FROM account WHERE id = p_id;\n"
		}, "CREATE OR REPLACE FUNCTION"},
		{"view definition", func(s *schema.Schema) {
			s.Views[0].Query = "SELECT id, email FROM account WHERE NOT active"
		}, "VIEW"},
		{"trigger timing", func(s *schema.Schema) {
			s.Triggers[0].Create = "CREATE TRIGGER account_ins AFTER UPDATE ON account " +
				"REFERENCING NEW TABLE AS newtab FOR EACH STATEMENT EXECUTE FUNCTION note_change()"
		}, "TRIGGER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := withRoutines()
			tc.mutate(s)
			p, err := migrate.ForWith(ctx, c, "storm_rt_changed", s, migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if p.Empty() {
				t.Fatalf("a changed %s produced no plan", tc.name)
			}
			if !strings.Contains(p.SQL(), tc.expect) {
				t.Fatalf("plan for a changed %s does not mention %s:\n%s", tc.name, tc.expect, p.SQL())
			}
		})
	}
}

// An extension's functions are not the adopter's. pg_trgm puts ~15 of them in
// whichever namespace it is installed into; without the pg_depend filter storm
// introspects them, finds no declaration, and plans to DROP them.
func TestIntrospectIgnoresExtensionOwnedFunctions(t *testing.T) {
	ctx, c := conn(t, "storm_rt_ext")
	if _, err := c.Exec(ctx, "CREATE SCHEMA storm_rt_ext"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA storm_rt_ext"); err != nil {
		t.Skipf("pg_trgm unavailable: %v", err)
	}
	plan, err := migrate.ForWith(ctx, c, "storm_rt_ext", &schema.Schema{}, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.SQL(), "DROP FUNCTION") {
		t.Fatalf("plan drops an extension's functions:\n%s", plan.SQL())
	}
}
