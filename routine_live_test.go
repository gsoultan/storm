package storm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
)

// The adopter's path, end to end: declare a function, a view and a trigger
// beside the models, build, apply, and ask for the migration again. A model
// that does not round-trip makes `storm verify` report drift no migration can
// clear, so this is the gate that says the feature is usable rather than
// merely present.

type acct struct {
	storm.Model
	Email  string
	Active bool
}

func (a *acct) Schema(t *storm.Table) { t.Unique(&a.Email) }

type hit struct {
	storm.Model
	N int64
}

func routineModel() []any {
	return []any{
		&acct{}, &hit{},
		storm.Function("acct_email", "p_id uuid", "text").
			Language("sql").Stable().
			Body("\n  SELECT email FROM accts WHERE id = p_id;\n"),
		// A plpgsql body carrying dollar quotes of its own — the case that
		// turns a fixed quote tag into a syntax error.
		storm.Function("note_hit", "", "trigger").
			Body("\nBEGIN\n  -- written the way anybody writes one: $$\n" +
				"  INSERT INTO hits(n) SELECT count(*) FROM newtab;\n  RETURN NULL;\nEND;\n"),
		storm.View("active_acct", "SELECT id, email FROM accts WHERE active"),
		storm.Trigger("acct_ins", "accts",
			"CREATE TRIGGER acct_ins AFTER INSERT ON accts "+
				"REFERENCING NEW TABLE AS newtab FOR EACH STATEMENT "+
				"EXECUTE FUNCTION note_hit()"),
	}
}

func TestDeclaredRoutinesRoundTrip(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_decl_routines"

	s, err := storm.Build(routineModel()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Functions) != 2 || len(s.Views) != 1 || len(s.Triggers) != 1 {
		t.Fatalf("Build dropped declarations: %d functions, %d views, %d triggers",
			len(s.Functions), len(s.Views), len(s.Triggers))
	}

	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	})
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}

	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.SQL(), "CREATE OR REPLACE FUNCTION") {
		t.Fatalf("plan has no function:\n%s", plan.SQL())
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatalf("plan does not apply: %v\n---\n%s", err, plan.SQL())
	}

	again, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("declared routines do not round-trip:\n%s", again.SQL())
	}
}

// A declared function whose body reads a column that is not there must fail at
// GENERATION time. This is the property that makes a text body acceptable at
// all: the text is checked against a real server before it can ship.
func TestABadFunctionBodyFailsGeneration(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_bad_routine"
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	})
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}

	s, err := storm.Build(&acct{},
		storm.Function("broken", "p_id uuid", "text").
			Language("sql").Stable().
			Body("\n  SELECT no_such_column FROM accts WHERE id = p_id;\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err == nil {
		t.Fatal("a function reading a column that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "no_such_column") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}
