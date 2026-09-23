package orabench

// migrate's Oracle half, APPLIED.
//
// The same gate the SQL Server half has, and the same property: a plan is
// applied and the NEXT plan must be EMPTY. That is the only evidence that
// normalisation and introspection agree about widths, parenthesised defaults
// and the CHECK an enum became — a migration that reapplies itself forever is
// what disagreement looks like in production.
//
// Four statements are spelled differently here and three fail in a way no text
// assertion sees: MODIFY restates only what changed (repeating a NOT NULL is
// ORA-01442), a DEFAULT is a column property rather than a named constraint,
// DROP INDEX takes no ON clause, and a trailing semicolon is ORA-00911.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/sqldrv"
	"github.com/gsoultan/storm/schema"
)

type migOrg struct {
	storm.Model
	Name    string
	Seats   int32
	Balance storm.Decimal
	Active  bool
	Opened  time.Time
	Status  oraStatus
}

func (o *migOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Balance).Numeric(18, 4)
	t.Col(&o.Opened).Date()
	t.Col(&o.Seats).Default("1")
	t.Unique(&o.Name)
	t.Index(&o.Active)
}

func migModel(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&migOrg{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func migSetup(t *testing.T) (migrate.Conn, func()) {
	t.Helper()
	db := open(t)
	clean := func() { drop(db, "TABLE", `"mig_orgs" CASCADE CONSTRAINTS PURGE`) }
	clean()
	t.Cleanup(clean)
	return sqldrv.New(db), clean
}

func TestOracleMigrationRoundTrip(t *testing.T) {
	ex, _ := migSetup(t)
	want := migModel(t)

	first, err := migrate.ForOracle(ctxBG(), ex, "", want)
	if err != nil {
		t.Fatalf("ForOracle: %v", err)
	}
	if first.Empty() {
		t.Fatal("an empty schema produced no plan")
	}
	applyOra(t, ex, first)

	// The property everything else rests on.
	again, err := migrate.ForOracle(ctxBG(), ex, "", want)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("re-diff after applying is not empty — normalisation and introspection "+
			"disagree:\n%s", again.SQL())
	}
}

// Every renderer whose Oracle spelling differs, applied. Each step is its own
// plan so a failure names the statement the server refused.
func TestOracleAltersApply(t *testing.T) {
	ex, _ := migSetup(t)
	first, err := migrate.ForOracle(ctxBG(), ex, "", migModel(t))
	if err != nil {
		t.Fatal(err)
	}
	applyOra(t, ex, first)

	steps := []struct {
		name  string
		edit  func(*schema.Table)
		wants string
	}{
		// NOT NULL with a default, because the empty-string rule refuses a
		// nullable text column — and a NOT NULL column added to a table that
		// may have rows needs one. The rule firing on the first draft of this
		// test is the rule working.
		{"add a column", func(tb *schema.Table) {
			tb.Columns = append(tb.Columns, &schema.Column{
				Name: "region", Type: schema.Type{Name: schema.TypeVarchar, Size: 40},
				NotNull: true, Default: "'x'",
			})
		}, `ADD "region"`},

		{"widen a column", func(tb *schema.Table) {
			tb.Column("name").Type.Size = 300
		}, `MODIFY ("name"`},

		// The nullability steps use a NUMBER, for the same reason: a text
		// column cannot legally become nullable on this target.
		{"make a column nullable", func(tb *schema.Table) {
			tb.Column("seats").NotNull = false
		}, `MODIFY ("seats" NULL)`},

		{"make it NOT NULL again", func(tb *schema.Table) {
			tb.Column("seats").NotNull = true
		}, `MODIFY ("seats" NOT NULL)`},

		{"replace a default", func(tb *schema.Table) {
			tb.Column("seats").Default = "2"
		}, `MODIFY ("seats" DEFAULT 2)`},

		{"drop a default", func(tb *schema.Table) {
			tb.Column("seats").Default = ""
		}, `DEFAULT NULL`},

		{"drop an index", func(tb *schema.Table) {
			tb.Indexes = nil
		}, "DROP INDEX"},

		{"drop a unique constraint", func(tb *schema.Table) {
			tb.Uniques = nil
		}, "DROP CONSTRAINT"},

		{"drop a column", func(tb *schema.Table) {
			cols := tb.Columns[:0]
			for _, c := range tb.Columns {
				if c.Name != "region" {
					cols = append(cols, c)
				}
			}
			tb.Columns = cols
		}, `DROP COLUMN "region"`},
	}

	var edits []func(*schema.Table)
	for _, st := range steps {
		edits = append(edits, st.edit)
		t.Run(st.name, func(t *testing.T) {
			want := migModel(t)
			tb := want.Table("mig_orgs")
			if tb == nil {
				t.Fatal("mig_orgs is not in the model")
			}
			for _, e := range edits {
				e(tb)
			}
			p, err := migrate.ForOracle(ctxBG(), ex, "", want)
			if err != nil {
				t.Fatalf("ForOracle: %v", err)
			}
			if !strings.Contains(p.SQL(), st.wants) {
				t.Fatalf("plan does not contain %q, so this step tested nothing:\n%s",
					st.wants, p.SQL())
			}
			applyOra(t, ex, p)
			again, err := migrate.ForOracle(ctxBG(), ex, "", want)
			if err != nil {
				t.Fatal(err)
			}
			if !again.Empty() {
				t.Fatalf("re-diff after applying is not empty:\n%s", again.SQL())
			}
		})
	}
}

// Normalisation must leave NOTHING behind: the scratch names live in the
// connected user's OWN schema, so a leak is a table in the application's
// namespace rather than a database nobody looks at.
func TestNormalisationLeavesNoScratchObjects(t *testing.T) {
	ex, _ := migSetup(t)
	if _, err := migrate.NormalizeOracle(ctxBG(), ex, migModel(t)); err != nil {
		t.Fatal(err)
	}
	got, err := migrate.NormalizeOracle(ctxBG(), ex, migModel(t))
	if err != nil {
		t.Fatalf("a second normalisation failed, so the first left something behind: %v", err)
	}
	if got.Table("mig_orgs") == nil {
		t.Error("the normalised model lost its table")
	}
	// And the application's own schema is untouched by it.
	rows, err := ex.Query(ctxBG(),
		`SELECT table_name FROM user_tables WHERE table_name LIKE 'sn/_%' ESCAPE '/'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var leaked []string
	for rows.Next() {
		leaked = append(leaked, valStr(rows.Values()[0]))
	}
	if len(leaked) > 0 {
		t.Errorf("normalisation left %v in the caller's own schema", leaked)
	}
}

func applyOra(t *testing.T, ex migrate.Conn, p migrate.Plan) {
	t.Helper()
	for i, ch := range p.Changes {
		if strings.HasPrefix(strings.TrimSpace(ch.SQL), "--") {
			continue
		}
		// One Exec per STATEMENT: a Change may hold a CREATE TABLE and its
		// indexes, separated by newlines, and the protocol takes one at a time.
		for _, st := range strings.Split(ch.SQL, "\nCREATE ") {
			st = strings.TrimSpace(st)
			if st == "" {
				continue
			}
			if !strings.HasPrefix(st, "CREATE") && !strings.HasPrefix(st, "ALTER") &&
				!strings.HasPrefix(st, "DROP") {
				st = "CREATE " + st
			}
			if _, err := ex.Exec(ctxBG(), st, nil); err != nil {
				t.Fatalf("change %d refused by the server:\n%s\n  %v", i, st, err)
			}
		}
	}
}

func ctxBG() context.Context { return context.Background() }

func valStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
