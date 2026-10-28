package migrate_test

import (
	"errors"
	"testing"

	"github.com/gsoultan/storm/migrate"
)

// A model that shares its schema with tables something else owns: without
// DeclaredOnly the other table is a DROP and the plan is refused; with it the
// table is not part of the plan at all, and the model's own tables are made.
func TestAuto_DeclaredOnlyLeavesOtherTablesAlone(t *testing.T) {
	ns := "storm_auto_declared_only"
	ctx, c := conn(t, ns)

	other := sch(tbl("not_ours", col("id", "uuid", true), col("note", "text", false)))
	if _, err := migrate.Auto(ctx, c, other, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}

	var de *migrate.DestructiveError
	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns}); !errors.As(err, &de) {
		t.Fatalf("without DeclaredOnly: %v, want *DestructiveError for the other table", err)
	}

	res, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns, DeclaredOnly: true})
	if err != nil {
		t.Fatalf("Auto with DeclaredOnly: %v", err)
	}
	if !tableExists(t, ctx, c, ns, "users") {
		t.Fatalf("users was not created; Auto said it applied:\n%s", res.SQL())
	}
	if !tableExists(t, ctx, c, ns, "not_ours") {
		t.Fatal("DeclaredOnly dropped a table the model does not declare")
	}

	again, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns, DeclaredOnly: true})
	if err != nil || !again.Empty() {
		t.Fatalf("second Auto with DeclaredOnly: %v, applied %d step(s)", err, len(again.Applied))
	}
}
