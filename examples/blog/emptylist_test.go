package blog_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/examples/blog/store/author"
)

// A list predicate called with no values is the empty set, not NULL: NotIn
// keeps every row and In keeps none. Spreading a slice that happens to be
// empty is how a caller writes it, so it has to mean what it says.
func TestAListPredicateWithNoValuesIsTheEmptySet(t *testing.T) {
	ctx := context.Background()
	na := author.Create()
	na.SetName("Edsger")
	na.SetEmail("edsger@example.com")
	edsger, err := na.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	// The tour counts its own authors, and the tests run in any order.
	t.Cleanup(func() { _ = author.Delete(context.Background(), ex, edsger.ID) })
	total, err := author.New().Count(ctx, ex)
	if err != nil || total == 0 {
		t.Fatalf("counted %d authors (%v), want at least the one just written", total, err)
	}
	var none [][16]byte
	if got, err := author.New().Where(author.ID.NotIn(none...)).Count(ctx, ex); err != nil || got != total {
		t.Errorf("NotIn() kept %d of %d rows (%v), want every row", got, total, err)
	}
	if got, err := author.New().Where(author.ID.In(none...)).Count(ctx, ex); err != nil || got != 0 {
		t.Errorf("In() kept %d rows (%v), want none", got, err)
	}
}
