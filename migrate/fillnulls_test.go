package migrate_test

import (
	"strings"
	"testing"

	"errors"

	"github.com/gsoultan/storm/migrate"
)

// A nullable column made NOT NULL with FillNulls is filled first and then
// tightened, as one plan Auto accepts without AllowDestructive; the rows that
// were NULL hold the declared value and the others keep theirs.
func TestAuto_FillNullsTightensAColumnThatHasNulls(t *testing.T) {
	ns := "storm_auto_fillnulls"
	ctx, c := conn(t, ns)

	before := sch(tbl("notes", col("id", "uuid", true), col("title", "text", false)))
	if _, err := migrate.Auto(ctx, c, before, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+ns+".notes VALUES (gen_random_uuid(), NULL), (gen_random_uuid(), 'kept')"); err != nil {
		t.Fatal(err)
	}

	after := sch(tbl("notes", col("id", "uuid", true), col("title", "text", true)))
	var de *migrate.DestructiveError
	if _, err := migrate.Auto(ctx, c, after, migrate.AutoOptions{Schema: ns}); !errors.As(err, &de) {
		t.Fatalf("tightening without FillNulls: %v, want *DestructiveError", err)
	}

	after.Tables[0].Columns[1].FillNulls = "''"
	res, err := migrate.Auto(ctx, c, after, migrate.AutoOptions{Schema: ns})
	if err != nil {
		t.Fatalf("Auto with FillNulls: %v", err)
	}
	if !strings.Contains(res.SQL(), `UPDATE "notes" SET "title" = '' WHERE "title" IS NULL`) {
		t.Fatalf("the plan does not fill the NULLs:\n%s", res.SQL())
	}
	var empty, kept int
	if err := c.QueryRow(ctx, "SELECT count(*) FILTER (WHERE title = ''), count(*) FILTER (WHERE title = 'kept') FROM "+ns+".notes").
		Scan(&empty, &kept); err != nil {
		t.Fatal(err)
	}
	if empty != 1 || kept != 1 {
		t.Fatalf("after the fill %d row(s) hold '' and %d 'kept'; want one each", empty, kept)
	}
	var nullable string
	if err := c.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
		 WHERE table_schema=$1 AND table_name='notes' AND column_name='title'`, ns).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatal("the column is still nullable")
	}
}
