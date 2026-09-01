package orders_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/runtime"

	"example.com/orders/store/product"
)

// Full-text search: the tsvector is maintained by the database and queried
// through the generated API, with the term BOUND — never interpolated.
func TestFullTextSearch(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-BLUE", "1.00", 1)
	seed(t, "SKU-BRASS", "1.00", 1)

	// The generated Search column indexes name + sku; the seed names products
	// "Widget <sku>".
	hits, err := product.New().Where(product.Search.Matches("widget")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("full-text search matched nothing")
	}

	// A term that appears in one product only.
	one, err := product.New().Where(product.Search.Matches("SKU-BLUE")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Errorf("a unique term matched %d products, want 1", len(one))
	}
	if len(one) == 1 && one[0].Sku != "SKU-BLUE" {
		t.Errorf("matched %s", one[0].Sku)
	}

	// A term in nothing.
	none, err := product.New().Where(product.Search.Matches("kumquat")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("a term in no product matched %d", len(none))
	}
}

// websearch_to_tsquery understands what a search box produces: quoted phrases,
// OR, and a leading minus. plainto_tsquery ignores all of it.
func TestWebSearchSyntax(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-ALPHA", "1.00", 1)
	seed(t, "SKU-BETA", "1.00", 1)

	both, err := product.New().Where(product.Search.WebSearch("SKU-ALPHA or SKU-BETA")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) < 2 {
		t.Errorf("an OR search matched %d products, want at least 2", len(both))
	}

	// Negation: widgets that are NOT alpha.
	not, err := product.New().Where(product.Search.WebSearch("widget -SKU-ALPHA")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range not {
		if p.Sku == "SKU-ALPHA" {
			t.Error("a negated term was returned")
		}
	}
}

// The search term is a BOUND parameter. A term full of tsquery metacharacters
// must be data, not syntax — and must not error, because it came from a user.
func TestSearchTermIsBoundNotInterpolated(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-SAFE", "1.00", 1)

	for _, term := range []string{
		"widget'); DROP TABLE products; --",
		"a & b | c ! d",
		"'quoted'",
		"",
	} {
		if _, err := product.New().Where(product.Search.Matches(term)).All(ctx, ex, nil); err != nil {
			t.Errorf("term %q errored: %v", term, err)
		}
	}
	// The table is still there.
	if _, err := product.New().All(ctx, ex, nil); err != nil {
		t.Fatalf("products is gone: %v", err)
	}
}

// Two searches differing only in the TERM share one compiled statement: the
// term is a value, so it is not part of the shape.
func TestSearchSharesOneStatement(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-SHAPE", "1.00", 1)
	for _, term := range []string{"alpha", "beta", "gamma"} {
		if _, err := product.New().Where(product.Search.Matches(term)).All(ctx, ex, nil); err != nil {
			t.Fatal(err)
		}
	}
	a := product.New().Where(product.Search.Matches("alpha")).SQL()
	b := product.New().Where(product.Search.Matches("beta")).SQL()
	if a != b {
		t.Errorf("two terms produced different statements:\n%s\n%s", a, b)
	}
}

// The optional-filter idiom. WhereIf cannot do this: it takes a built Pred, so
// the caller must dereference the pointer BEFORE the condition is tested,
// which panics on exactly the nil the condition was checking for.
func TestWhenSetSkipsNilFilters(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-OPT", "9.00", 1)

	type filters struct {
		MinPrice *runtime.Decimal
		Sku      *string
	}
	apply := func(f filters) ([]product.Row, error) {
		q := product.New()
		q = product.WhenSet(q, f.MinPrice, product.Price.Gte)
		q = product.WhenSet(q, f.Sku, product.Sku.Eq)
		return q.All(ctx, ex, nil)
	}

	// Nothing set: no filter, no dereference, no panic.
	all, err := apply(filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no products")
	}

	// One set.
	sku := "SKU-OPT"
	one, err := apply(filters{Sku: &sku})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Sku != sku {
		t.Errorf("got %d rows", len(one))
	}

	// Both set.
	min := mustDec(t, "5.00")
	both, err := apply(filters{Sku: &sku, MinPrice: &min})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 {
		t.Errorf("got %d rows, want 1", len(both))
	}
}
