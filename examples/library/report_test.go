package library_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/examples/library/store"
	"github.com/gsoultan/storm/examples/library/store/author"
	"github.com/gsoultan/storm/examples/library/store/book"
	"github.com/gsoultan/storm/examples/library/store/volume"
	"github.com/gsoultan/storm/runtime"
)

// A declared aggregation is one $group, with SQL's answers: a count of a
// column skips NULLs, the average of integers is a decimal rounded to six
// places, NULL sorts where SQL puts it, and with no grouping columns there
// is one row even over nothing.
func TestAggregations(t *testing.T) {
	ctx := context.Background()
	a, b := runtime.NewUUIDv7(), runtime.NewUUIDv7()
	published := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, x := range []struct {
		author [16]byte
		pages  int32
		price  int64
		isbn   bool
		when   bool
	}{
		{a, 100, 1000, true, true},
		{a, 101, 550, false, false},
		{a, 100, 125, false, false},
		{b, 3, 10, false, false},
	} {
		nb := book.Create()
		nb.SetTitle("Agg")
		nb.SetAuthorID(x.author)
		nb.SetPages(x.pages)
		nb.SetPrice(runtime.Decimal{Unscaled: x.price, Scale: 2})
		if x.isbn {
			nb.SetIsbn("isbn-agg-" + time.Now().Format("150405.000000"))
		}
		if x.when {
			nb.SetPublishedAt(published)
		}
		if _, err := nb.Insert(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	q := book.New().Where(book.AuthorID.In(a, b))
	rows, err := q.AllByAuthor(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("groups: %+v", rows)
	}
	ra, rb := rows[0], rows[1]
	dec := func(u int64, s int32) runtime.Null[runtime.Decimal] {
		return runtime.Null[runtime.Decimal]{V: runtime.Decimal{Unscaled: u, Scale: s}, Valid: true}
	}
	if ra.AuthorID != a || ra.Books != 3 || ra.WithISBN != 1 || ra.Lengths != 2 ||
		ra.Pages != (runtime.Null[int64]{V: 301, Valid: true}) ||
		ra.AvgPages != dec(100333333, 6) || ra.Value != dec(1675, 2) || ra.Dearest != dec(1000, 2) ||
		!ra.First.Valid || !ra.First.V.Equal(published) {
		t.Fatalf("the first author's row: %+v", ra)
	}
	if rb.AuthorID != b || rb.Books != 1 || rb.WithISBN != 0 || rb.AvgPages != dec(3000000, 6) || rb.First.Valid {
		t.Fatalf("the second author's row, ordered after by pages: %+v", rb)
	}
	if page, err := q.Offset(1).Limit(1).AllByAuthor(ctx, db); err != nil || len(page) != 1 || page[0].AuthorID != b {
		t.Fatalf("paging the groups: %+v %v", page, err)
	}

	// No grouping columns: one row over no documents, the count 0 and the
	// average NULL — and none past it.
	none := book.New().Where(book.Title.Eq("no such book"))
	if tot, err := none.AllTotals(ctx, db); err != nil || len(tot) != 1 || tot[0].Books != 0 || tot[0].AvgPrice.Valid {
		t.Fatalf("totals over nothing: %+v %v", tot, err)
	}
	if tot, err := none.Offset(1).AllTotals(ctx, db); err != nil || len(tot) != 0 {
		t.Fatalf("totals past the one row: %+v %v", tot, err)
	}
	if tot, err := q.AllTotals(ctx, db); err != nil || len(tot) != 1 || tot[0].Books != 4 || tot[0].AvgPrice != dec(4212500, 6) {
		t.Fatalf("totals: %+v %v", tot, err)
	}
	if _, err := q.Order(book.Title.Asc()).AllByAuthor(ctx, db); err == nil {
		t.Fatal("an aggregation's rows are groups; Order on one must be refused")
	}
}

// A NULL grouping column is one group, sorted last ascending as SQL sorts
// it; an int8 sum is a decimal, as PostgreSQL's numeric is.
func TestAggregationGroupsNull(t *testing.T) {
	ctx := context.Background()
	donor := runtime.NewUUIDv7()
	for i, x := range []struct {
		donor bool
		n     int64
	}{{true, 2}, {false, 5}, {true, 3}} {
		nv := volume.Create()
		nv.SetBranch("agg")
		nv.SetBarcode("agg-" + string(rune('a'+i)))
		nv.SetBookID(runtime.NewUUIDv7())
		nv.SetCheckouts(x.n)
		nv.SetOnShelf(false)
		if x.donor {
			nv.SetDonorID(donor)
		}
		if _, err := nv.Insert(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := volume.New().Where(volume.Branch.Eq("agg")).AllByDonor(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	five := runtime.Null[runtime.Decimal]{V: runtime.Decimal{Unscaled: 5}, Valid: true}
	if len(rows) != 2 || rows[0].DonorID.V != donor || rows[0].Copies != 2 || rows[0].Checkouts != five ||
		rows[1].DonorID.Valid || rows[1].Copies != 1 || rows[1].Checkouts != five {
		t.Fatalf("by donor: %+v", rows)
	}
}

// A declared join is a $lookup per table with SQL's NULL: a copy with no
// donor, or whose donor was removed, keeps its row with no name and sorts
// last; a copy whose book is missing has no row.
func TestJoinedShelf(t *testing.T) {
	ctx := context.Background()
	nb := book.Create()
	nb.SetTitle("Joined")
	nb.SetPages(42)
	nb.SetPrice(runtime.Decimal{Unscaled: 1})
	nb.SetAuthorID(runtime.NewUUIDv7())
	bk, err := nb.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	donors := map[string][16]byte{}
	for _, name := range []string{"Zed", "Ada"} {
		na := author.Create()
		na.SetName(name)
		na.SetEmail(name + "@join.example.com")
		a, err := na.Insert(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		donors[name] = a.ID
	}
	gone := donors["Zed"]
	for _, v := range []struct {
		code  string
		book  [16]byte
		donor *[16]byte
		shelf bool
	}{
		{"j1", bk.ID, &gone, true},
		{"j2", bk.ID, nil, true},
		{"j3", bk.ID, ptr(donors["Ada"]), true},
		{"j4", runtime.NewUUIDv7(), nil, true}, // no such book
		{"j5", bk.ID, nil, false},              // off the shelf
	} {
		nv := volume.Create()
		nv.SetBranch("join")
		nv.SetBarcode(v.code)
		nv.SetBookID(v.book)
		nv.SetOnShelf(v.shelf)
		if v.donor != nil {
			nv.SetDonorID(*v.donor)
		}
		if _, err := nv.Insert(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if err := author.Delete(ctx, db, gone); err != nil { // soft: the document stays
		t.Fatal(err)
	}
	rows, err := volume.New().Where(volume.Branch.Eq("join")).AllShelved(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Barcode+":"+r.Donor.V)
		if r.Title != "Joined" || r.Pages != 42 {
			t.Fatalf("the book's side: %+v", r)
		}
	}
	if strings.Join(got, " ") != "j3:Ada j1: j2:" {
		t.Fatalf("shelved: %v", got)
	}
	if page, err := volume.New().Where(volume.Branch.Eq("join")).Offset(1).Limit(1).AllShelved(ctx, db); err != nil ||
		len(page) != 1 || page[0].Barcode != "j1" {
		t.Fatalf("paging the join: %+v %v", page, err)
	}
}

func ptr[T any](v T) *T { return &v }

// A declared union is one aggregation over the first branch's collection,
// the rest $unionWith, ordered and capped as one.
func TestUnionFeed(t *testing.T) {
	ctx := context.Background()
	na := author.Create()
	na.SetName("Feeder")
	na.SetEmail("feeder@union.example.com")
	a, err := na.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		title string
		pages int32
	}{{"Thin", 5}, {"First", 100}, {"Second", 200}} {
		time.Sleep(2 * time.Millisecond) // created_at is the server's clock, in milliseconds
		nb := book.Create()
		nb.SetTitle(x.title)
		nb.SetPages(x.pages)
		nb.SetPrice(runtime.Decimal{Unscaled: 1})
		nb.SetAuthorID(a.ID)
		if _, err := nb.Insert(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	feed, err := store.Activity(ctx, db, a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range feed {
		got = append(got, r.Kind+":"+r.Text)
	}
	if strings.Join(got, " ") != "book:Second book:First author:Feeder" {
		t.Fatalf("feed: %v", got)
	}
	if two, err := store.Activity(ctx, db, a.ID, 2); err != nil || len(two) != 2 {
		t.Fatalf("the cap applies to the merge: %+v %v", two, err)
	}
	if err := author.Delete(ctx, db, a.ID); err != nil {
		t.Fatal(err)
	}
	if feed, err := store.Activity(ctx, db, a.ID, 10); err != nil || len(feed) != 2 {
		t.Fatalf("a removed author's own entry leaves the feed: %+v %v", feed, err)
	}
}
