package migrate_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
)

func TestReverse_UndoesTheForwardPlan(t *testing.T) {
	from := users()
	to := users(col("bio", "text", false))
	to.Tables = append(to.Tables, tbl("tags", col("id", "uuid", true)))

	up := migrate.Diff(from, to)
	down, err := up.Reverse()
	if err != nil {
		t.Fatal(err)
	}
	got := down.SQL()
	for _, w := range []string{
		`ALTER TABLE "users" DROP COLUMN "bio";`,
		`DROP TABLE "tags";`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("down lacks %s:\n%s", w, got)
		}
	}
	if !down.Destructive() {
		t.Error("a down that drops what the up created loses the rows written since; it must say so")
	}
	// And the reverse of the reverse is the forward plan.
	again, err := down.Reverse()
	if err != nil {
		t.Fatal(err)
	}
	if again.SQL() != up.SQL() {
		t.Errorf("reversing twice changed the plan:\n%s\nwant:\n%s", again.SQL(), up.SQL())
	}
}

func TestReverse_KeepsTheOnlineForm(t *testing.T) {
	from := users()
	to := users()
	to.Tables[0].Indexes = []*schema.Index{{Name: "ix_users_email", Columns: []schema.IndexColumn{{Name: "email"}}}}
	up := migrate.Diff(from, to).Concurrently(from)
	down, err := up.Reverse()
	if err != nil {
		t.Fatal(err)
	}
	if got := down.SQL(); !strings.Contains(got, `DROP INDEX CONCURRENTLY "ix_users_email";`) {
		t.Fatalf("an online up has an online down; got:\n%s", got)
	}
}

func TestReverse_RefusesAHandBuiltPlan(t *testing.T) {
	if _, err := (migrate.Plan{Changes: []migrate.Change{{SQL: "SELECT 1;"}}}).Reverse(); err == nil {
		t.Fatal("a plan with nothing to reverse from was reversed")
	}
}
