package orders_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gsoultan/storm/runtime"

	"example.com/orders/model"
	"example.com/orders/store/customer"
	"example.com/orders/store/product"
	"example.com/orders/store/stockitem"
)

// The list and pattern predicates, against a real server.
//
// A list predicate binds one Go slice to one placeholder, and the slice's
// element type has to reach PostgreSQL as the array type the column is
// compared against. That is a wire-format question no unit test answers: the
// generated method compiles either way, and a list bound into the wrong slot
// is a query that runs and returns the wrong rows. storm has had exactly that
// bug once already — a bool riding the int64 arena, which no fixture had a
// column for — so a new slot without a live test is the same mistake twice.

func TestIntListPredicates(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-N7", "1.00", 7)
	seed(t, "SKU-N11", "1.00", 11)
	seed(t, "SKU-N13", "1.00", 13)

	// int32 is its own slot. Before it existed, opApplies claimed integers
	// supported In and no method was emitted for them: `WHERE on_hand IN
	// (7, 13)` could not be written at all.
	in, err := stockitem.New().Where(stockitem.OnHand.In(7, 13)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int32]bool{}
	for _, s := range in {
		got[s.OnHand] = true
	}
	if !got[7] || !got[13] || got[11] {
		t.Errorf("In(7, 13) matched on_hand %v; want exactly 7 and 13", keys(got))
	}

	// NotIn is `<> ALL`, and 11 is the only one of the three left.
	out, err := stockitem.New().
		Where(stockitem.OnHand.NotIn(7, 13), stockitem.OnHand.Lte(13)).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out {
		if s.OnHand == 7 || s.OnHand == 13 {
			t.Errorf("NotIn(7, 13) returned on_hand %d", s.OnHand)
		}
	}
}

// One statement for every list length is the property In exists for: the list
// is ONE bound argument, so a query for two ids and a query for two hundred
// share a plan rather than filling the cache with one statement per length.
func TestListLengthDoesNotChangeTheStatement(t *testing.T) {
	b := stockitem.GetBinder()
	defer stockitem.PutBinder(b)
	two, _ := stockitem.New().Where(stockitem.OnHand.In(1, 2)).Prepare(b)
	many, _ := stockitem.New().Where(stockitem.OnHand.In(1, 2, 3, 4, 5, 6, 7, 8)).Prepare(b)
	if two != many {
		t.Errorf("list length changed the statement:\n  2: %s\n  8: %s", two, many)
	}
}

func TestTextListAndPatternPredicates(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-LIKE-A", "1.00", 1)
	seed(t, "SKU-LIKE-B", "1.00", 1)

	emails := []string{"SKU-LIKE-A@example.com", "SKU-LIKE-B@example.com"}
	rows, err := customer.New().Where(customer.Email.In(emails...)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("In over two emails matched %d rows, want 2", len(rows))
	}

	// ILIKE is the point: LIKE with this pattern matches nothing, because the
	// stored value is upper-case. Someone writing lower(email) LIKE lower($1)
	// instead gets the same answer with the index thrown away.
	ci, err := customer.New().Where(customer.Email.ILike("sku-like-%@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ci) != 2 {
		t.Errorf("ILike matched %d rows, want 2", len(ci))
	}
	cs, err := customer.New().Where(customer.Email.Like("sku-like-%@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("LIKE matched %d rows case-insensitively; the two operators are the same", len(cs))
	}
}

// A NULL in the list makes `<> ALL` NULL for every row, so the result is
// empty. That is PostgreSQL's rule for NOT IN and storm does not paper over
// it — but it is only safe to leave undocumented if it is also true, and this
// is the only place that says so.
func TestNotInIsUnaffectedByOrderOfArguments(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-ORD-1", "1.00", 41)
	seed(t, "SKU-ORD-2", "1.00", 43)

	a, err := stockitem.New().Where(stockitem.OnHand.NotIn(41), stockitem.OnHand.Gte(41)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stockitem.New().Where(stockitem.OnHand.Gte(41), stockitem.OnHand.NotIn(41)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("predicate order changed the result: %d vs %d", len(a), len(b))
	}
	for _, s := range a {
		if s.OnHand == 41 {
			t.Error("NotIn(41) returned a row with on_hand 41")
		}
	}
}

func keys(m map[int32]bool) []int32 {
	out := make([]int32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Array containment and overlap, against a real server.
//
// Before these operators an array column round-tripped and could only be
// tested for NULL: storable, not filterable. That is the shape of gap that
// sends a caller to raw SQL for a question the model already describes, and it
// is worse than a missing type, because the column looks supported.
//
// Equality is still refused. `tags = '{a,b}'` is order- and
// duplicate-sensitive, which almost nobody means.
func TestArrayPredicates(t *testing.T) {
	ctx := context.Background()
	// tags is passed as a slice rather than variadic on purpose: an empty
	// variadic is a NIL slice, and storm keeps nil and empty distinct — nil is
	// SQL NULL, which this NOT NULL column refuses. `{}` is the empty array.
	tag := func(sku string, tags []string) {
		t.Helper()
		n := product.Create()
		n.SetSku(sku)
		n.SetName("Widget " + sku)
		n.SetPrice(mustDec(t, "1.00"))
		n.SetTags(tags)
		if _, err := n.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}
	tag("SKU-ARR-SALE", []string{"sale", "new"})
	tag("SKU-ARR-CLEAR", []string{"clearance"})
	tag("SKU-ARR-BOTH", []string{"sale", "clearance"})
	tag("SKU-ARR-NONE", []string{})

	skus := func(rows []product.Row) map[string]bool {
		m := map[string]bool{}
		for _, r := range rows {
			m[r.Sku] = true
		}
		return m
	}

	// @> — every element of the argument is in the column. Both tags, so only
	// the row carrying both.
	both, err := product.New().Where(product.Tags.Contains("sale", "clearance")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := skus(both)
	if !got["SKU-ARR-BOTH"] || got["SKU-ARR-SALE"] || got["SKU-ARR-CLEAR"] {
		t.Errorf(`Contains("sale","clearance") matched %v; want only SKU-ARR-BOTH`, got)
	}

	// && — shares at least one element. Three of the four.
	any, err := product.New().Where(product.Tags.Overlaps("sale", "clearance")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	got = skus(any)
	for _, want := range []string{"SKU-ARR-SALE", "SKU-ARR-CLEAR", "SKU-ARR-BOTH"} {
		if !got[want] {
			t.Errorf(`Overlaps("sale","clearance") missed %s`, want)
		}
	}
	if got["SKU-ARR-NONE"] {
		t.Error(`Overlaps matched a product with no tags`)
	}

	// <@ — every element of the column is in the argument. The empty-tag row
	// qualifies, which is the correct and easily-surprising answer.
	sub, err := product.New().
		Where(product.Tags.ContainedBy("sale", "clearance", "new"), product.Sku.Like("SKU-ARR-%")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !skus(sub)["SKU-ARR-NONE"] {
		t.Error("ContainedBy excluded the empty array; every element of {} is in any set")
	}
}

// An array column shares the text list slot with In on a plain text column.
// One Pred sets one slot, but the bind chain has to reach the right one — and
// a query mixing both in a single statement is where a wrong chain shows up.
func TestArrayAndScalarListInOneQuery(t *testing.T) {
	ctx := context.Background()
	n := product.Create()
	n.SetSku("SKU-MIX-1")
	n.SetName("Widget mix")
	n.SetPrice(mustDec(t, "1.00"))
	n.SetTags([]string{"sale"})
	if _, err := n.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}

	rows, err := product.New().
		Where(product.Sku.In("SKU-MIX-1", "SKU-MIX-2"), product.Tags.Overlaps("sale")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sku != "SKU-MIX-1" {
		t.Errorf("mixed list predicates returned %d rows: %v", len(rows), rows)
	}
}

// Two list predicates in one query.
//
// A regression test for a silent wrong answer present in every version through
// v0.3.0: Query held ONE field per list slot, so a second In on the same slot
// overwrote the first, and the bind loop then appended that one list for both
// placeholders. The query ran, returned the wrong rows, and reported nothing.
//
// List values are an arena now, indexed by a cursor in token order, exactly
// like every scalar value type — which is what makes more than one of them
// representable at all.
func TestTwoListPredicatesInOneQuery(t *testing.T) {
	ctx := context.Background()
	n := product.Create()
	n.SetSku("SKU-TWO-1")
	n.SetName("Widget two")
	n.SetPrice(mustDec(t, "1.00"))
	n.SetTags([]string{})
	if _, err := n.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// Both lists must reach the statement. Before the fix the second
	// overwrote the first, so `sku IN (...)` was bound with the NAME list and
	// matched nothing.
	rows, err := product.New().
		Where(product.Sku.In("SKU-TWO-1", "SKU-TWO-9"), product.Name.In("Widget two", "nope")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sku != "SKU-TWO-1" {
		t.Fatalf("two In predicates returned %d rows, want 1", len(rows))
	}

	// Order must not matter, and a non-matching second list must exclude.
	none, err := product.New().
		Where(product.Name.In("Widget two"), product.Sku.In("SKU-TWO-9")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("a non-matching second list returned %d rows; the lists are being crossed", len(none))
	}
}

// The list arena is bounded, and past the bound the query must ERROR rather
// than bind whatever was there. A limit that silently truncates is the bug
// this replaced, one layer down.
func TestTooManyListPredicatesIsAnError(t *testing.T) {
	ctx := context.Background()
	q := product.New().Where(
		product.Sku.In("a"), product.Name.In("b"),
		product.Sku.In("c"), product.Name.In("d"),
	)
	if _, err := q.All(ctx, ex, nil); err == nil {
		t.Error("four list predicates on a 3-slot arena were accepted silently")
	}
}

// jsonb containment and key tests, against a real server.
//
// Before these a jsonb column round-tripped and supported only IS [NOT] NULL:
// every question about the document went through raw SQL, on a column the
// model already describes. Equality is still refused — jsonb normalises key
// order and drops duplicates, so two documents a caller thinks differ can be
// equal and two they think match can differ by whitespace they never wrote.
func TestJSONBPredicates(t *testing.T) {
	ctx := context.Background()
	// The column is jsonb and its Go side is bytes: storm cannot know the
	// document's shape, so the caller marshals its own type. That is the same
	// contract on the way in as on the way out.
	add := func(sku string, a model.ProductAttrs) {
		t.Helper()
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		n := product.Create()
		n.SetSku(sku)
		n.SetName("Widget " + sku)
		n.SetPrice(mustDec(t, "1.00"))
		n.SetTags([]string{})
		n.SetAttrs(b)
		if _, err := n.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}
	add("SKU-J-RED", model.ProductAttrs{Colour: "red", SizeCM: 30})
	add("SKU-J-BLUE", model.ProductAttrs{Colour: "blue", SizeCM: 30})
	add("SKU-J-WIRE", model.ProductAttrs{Wireless: true})
	add("SKU-J-EMPTY", model.ProductAttrs{})

	skus := func(rows []product.Row) map[string]bool {
		m := map[string]bool{}
		for _, r := range rows {
			m[r.Sku] = true
		}
		return m
	}

	// @> — the document contains this one. The reason the column is queryable
	// at all, and what the GIN index answers.
	red, err := product.New().
		Where(product.Attrs.Contains(runtime.JSON(`{"colour":"red"}`))).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := skus(red)
	if !got["SKU-J-RED"] || got["SKU-J-BLUE"] {
		t.Errorf(`Contains({"colour":"red"}) matched %v`, got)
	}

	// Two keys at once: both must match, so neither single-colour row does.
	pair, err := product.New().
		Where(product.Attrs.Contains(runtime.JSON(`{"colour":"red","size_cm":30}`))).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := skus(pair); !g["SKU-J-RED"] || g["SKU-J-BLUE"] {
		t.Errorf("two-key containment matched %v", g)
	}

	// ?| — the document has at least one of these keys. omitempty means an
	// attribute that does not apply is ABSENT, which is what makes this ask a
	// real question rather than always being true.
	sized, err := product.New().
		Where(product.Attrs.HasAnyKey("size_cm"), product.Sku.Like("SKU-J-%")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := skus(sized)
	if !g["SKU-J-RED"] || !g["SKU-J-BLUE"] {
		t.Errorf(`HasAnyKey("size_cm") missed a sized product: %v`, g)
	}
	if g["SKU-J-WIRE"] || g["SKU-J-EMPTY"] {
		t.Errorf(`HasAnyKey("size_cm") matched a product without the key: %v`, g)
	}

	// ?& — every key present.
	both, err := product.New().
		Where(product.Attrs.HasAllKeys("colour", "size_cm"), product.Sku.Like("SKU-J-%")).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	g = skus(both)
	if !g["SKU-J-RED"] || !g["SKU-J-BLUE"] || g["SKU-J-WIRE"] {
		t.Errorf(`HasAllKeys("colour","size_cm") matched %v`, g)
	}
}

// A jsonb column spends its arena on the @> argument and its list slot on the
// key operators. Both in one query is where a shared cursor would show up.
func TestJSONBArenaAndListSlotInOneQuery(t *testing.T) {
	ctx := context.Background()
	n := product.Create()
	n.SetSku("SKU-J-MIX")
	n.SetName("Widget mix")
	n.SetPrice(mustDec(t, "1.00"))
	n.SetTags([]string{"sale"})
	attrs, err := json.Marshal(model.ProductAttrs{Colour: "green", SizeCM: 12})
	if err != nil {
		t.Fatal(err)
	}
	n.SetAttrs(attrs)
	if _, err := n.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}

	rows, err := product.New().Where(
		product.Attrs.Contains(runtime.JSON(`{"colour":"green"}`)),
		product.Attrs.HasAllKeys("colour", "size_cm"),
		product.Tags.Overlaps("sale"),
		product.Sku.In("SKU-J-MIX", "SKU-J-NOPE"),
	).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Sku != "SKU-J-MIX" {
		t.Fatalf("arena and list predicates together returned %d rows", len(rows))
	}
}
