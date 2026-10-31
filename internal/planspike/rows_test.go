package planspike_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// Rows sees what List sees, in the same order, and breaking out of the loop
// leaves the connection usable: the result was closed, not abandoned.
func TestRows_MatchesListAndBreakCloses(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	org := anOrg(t)
	for i := 0; i < 5; i++ {
		n := user.Create()
		n.SetEmail(fmt.Sprintf("rows-%02d@example.com", i))
		n.SetName("Rows")
		n.SetStatus("pending")
		n.SetOrgID(org)
		n.SetAge(30)
		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })
	}
	q := user.New().Where(user.Email.HasPrefix("rows-")).Order(user.Email.Asc())

	list, err := q.List(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("List returned %d rows, want 5", len(list))
	}
	n := 0
	for r, err := range q.Rows(ctx, ex) {
		if err != nil {
			t.Fatal(err)
		}
		if r.ID != list[n].ID {
			t.Fatalf("row %d: Rows read %v, List read %v", n, r.ID, list[n].ID)
		}
		n++
	}
	if n != len(list) {
		t.Fatalf("Rows visited %d rows, List returned %d", n, len(list))
	}

	for range q.Rows(ctx, ex) {
		break
	}
	if _, err := q.List(ctx, ex); err != nil {
		t.Fatalf("a read after breaking out of Rows failed: %v", err)
	}
}
