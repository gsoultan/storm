// The MongoDB example as a test: a generated package running against a real
// replica set through storm's own client. Every call below is the same call a
// PostgreSQL package takes.
//
//	docker run -d -p 27017:27017 mongo:7 --replSet rs0
//	docker exec <id> mongosh --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})'
//	STORM_MONGO_URI=mongodb://localhost:27017/storm_library go test ./examples/library
package library_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/mgddl"
	"github.com/gsoultan/storm/examples/library/model"
	"github.com/gsoultan/storm/examples/library/store/author"
	"github.com/gsoultan/storm/examples/library/store/book"
	"github.com/gsoultan/storm/examples/library/store/hold"
	"github.com/gsoultan/storm/examples/library/store/volume"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mgdrv"
)

var db *mgdrv.Pool

func TestMain(m *testing.M) {
	uri := os.Getenv("STORM_MONGO_URI")
	if uri == "" {
		fmt.Println("STORM_MONGO_URI unset; skipping the MongoDB example")
		os.Exit(0)
	}
	ctx := context.Background()
	var err error
	if db, err = mgdrv.OpenURI(ctx, uri); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The collections and their validators and indexes, exactly as
	// `storm ddl -dialect mongo` prints them.
	s, err := storm.Build(model.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmds, err := mgddl.Commands(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, c := range []string{author.Collection, book.Collection, volume.Collection, hold.Collection} {
		_, _ = db.Exec(ctx, `{"drop":"`+c+`"}`, nil)
	}
	for _, c := range cmds {
		if _, err := db.Exec(ctx, c, nil); err != nil {
			fmt.Fprintln(os.Stderr, c, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	db.Close()
	os.Exit(code)
}

func TestTour(t *testing.T) {
	ctx := context.Background()

	// Insert reads the stored document back: the key was made client-side
	// (MongoDB has no uuid generator a write can call) and the timestamps
	// came from the SERVER's clock.
	before := time.Now().Add(-time.Minute)
	n := author.Create()
	n.SetName("Ursula")
	n.SetEmail("ursula@example.com")
	ursula, err := n.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.IsZeroUUID(ursula.ID) || ursula.CreatedAt.Before(before) || ursula.Bio.Valid {
		t.Fatalf("insert read-back: %+v", ursula)
	}

	// The same unique index every target has; the same typed error.
	dup := author.Create()
	dup.SetName("Impostor")
	dup.SetEmail("ursula@example.com")
	if _, err := dup.Insert(ctx, db); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("duplicate email: %v", err)
	}

	octavia := author.Row{Name: "Octavia", Email: "octavia@example.com",
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond), UpdatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := author.Insert(ctx, db, &octavia); err != nil {
		t.Fatal(err)
	}

	price := func(cents int64) runtime.Decimal { return runtime.Decimal{Unscaled: cents, Scale: 2} }
	var books []book.Row
	for i, title := range []string{"The Dispossessed", "The Left Hand of Darkness", "A Wizard of Earthsea", "Kindred"} {
		b := book.Create()
		b.SetTitle(title)
		b.SetPages(int32(200 + 50*i))
		b.SetPrice(price(int64(999 + 100*i)))
		if i < 3 {
			b.SetAuthorID(ursula.ID)
		} else {
			b.SetAuthorID(octavia.ID)
			b.SetIsbn("978-0807083697")
		}
		row, err := b.Insert(ctx, db)
		if err != nil {
			t.Fatal(title, err)
		}
		books = append(books, row)
	}

	// Typed predicates, ordering, paging — one statement per SHAPE, whatever
	// the values.
	got, err := book.New().
		Where(book.AuthorID.Eq(ursula.ID), book.Pages.Gte(250)).
		Order(book.Pages.Desc()).
		List(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "A Wizard of Earthsea" || got[1].Price != price(1099) {
		t.Fatalf("filtered read: %+v", got)
	}
	shapes := book.Shapes()
	if _, err := book.New().Where(book.AuthorID.Eq(octavia.ID), book.Pages.Gte(1)).
		Order(book.Pages.Desc()).List(ctx, db); err != nil || book.Shapes() != shapes {
		t.Fatalf("same shape, other values, compiled again (%d → %d): %v", shapes, book.Shapes(), err)
	}

	// OR, In, text matches over escaped literals, and NULL the way SQL means it.
	cases := []struct {
		name string
		q    book.Query
		want int64
	}{
		{"any", book.New().Any(book.Title.HasPrefix("The "), book.Pages.Lt(210)), 2},
		{"in", book.New().Where(book.Title.In("Kindred", "Nope")), 1},
		{"contains is literal", book.New().Where(book.Title.Contains("of D")), 1},
		{"regex text is not a regex", book.New().Where(book.Title.Contains(".*")), 0},
		{"like", book.New().Where(book.Title.Like("%of%")), 2},
		{"ilike", book.New().Where(book.Title.ILike("the %")), 2},
		{"null", book.New().Where(book.Isbn.IsNull()), 3},
		{"not null", book.New().Where(book.Isbn.IsNotNull()), 1},
		// SQL's <> is UNKNOWN for NULL, so a row with no ISBN is not "not equal".
		{"noteq skips null", book.New().Where(book.Isbn.NotEq("x")), 1},
		{"notin skips null", book.New().Where(book.Isbn.NotIn("x")), 1},
	}
	for _, c := range cases {
		n, err := c.q.Count(ctx, db)
		if err != nil || n != c.want {
			t.Errorf("%s: count %d, want %d (%v)", c.name, n, c.want, err)
		}
	}
	if ok, err := book.New().Where(book.Title.Eq("Nope")).Exists(ctx, db); ok || err != nil {
		t.Errorf("exists on nothing: %v %v", ok, err)
	}

	// Paging walks the default order, the primary key.
	page1, _ := book.New().Limit(2).List(ctx, db)
	page2, _ := book.New().Limit(2).Offset(2).List(ctx, db)
	if len(page1) != 2 || len(page2) != 2 || page1[1].ID == page2[0].ID {
		t.Fatalf("paging: %d %d", len(page1), len(page2))
	}

	// Update writes only what was assigned and reads the stored row back.
	m := book.Mutate(books[0])
	m.SetPages(400)
	m.SetUpdatedAtNow()
	if err := m.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
	if r := m.Row(); r.Pages != 400 || r.Title != "The Dispossessed" || !r.UpdatedAt.After(books[0].UpdatedAt.Add(-time.Second)) {
		t.Fatalf("update read-back: %+v", r)
	}
	gone := book.MutateKey([16]byte{1})
	gone.SetPages(1)
	if err := gone.Update(ctx, db); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("update of a missing key: %v", err)
	}

	// Set-based writes need a predicate.
	if _, err := book.New().DeleteAll(ctx, db); err == nil {
		t.Fatal("DeleteAll with no predicate must be refused")
	}
	all := book.MutateKey([16]byte{})
	all.SetIsbnNull()
	if n, err := book.New().Where(book.AuthorID.Eq(octavia.ID)).UpdateAll(ctx, db, all); err != nil || n != 1 {
		t.Fatalf("UpdateAll: %d %v", n, err)
	}
	if n, _ := book.New().Where(book.Isbn.IsNull()).Count(ctx, db); n != 4 {
		t.Fatalf("after setting null, %d books lack an ISBN", n)
	}
	if n, err := book.New().Where(book.AuthorID.Eq(octavia.ID)).DeleteAll(ctx, db); err != nil || n != 1 {
		t.Fatalf("DeleteAll: %d %v", n, err)
	}
	if err := book.Delete(ctx, db, books[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := book.Delete(ctx, db, books[0].ID); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("second delete: %v", err)
	}

	// A transaction is the same runtime.InTx every target uses.
	boom := errors.New("boom")
	err = runtime.InTx(ctx, db, func(ex runtime.Executor) error {
		b := book.Create()
		b.SetTitle("Never")
		b.SetPages(1)
		b.SetPrice(price(1))
		b.SetAuthorID(octavia.ID)
		if _, err := b.Insert(ctx, ex); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if ok, _ := book.New().Where(book.Title.Eq("Never")).Exists(ctx, db); ok {
		t.Fatal("a rolled-back insert is visible")
	}

	// Bulk insert: one command per thousand documents.
	bulk := make([]book.Row, 2500)
	for i := range bulk {
		bulk[i] = book.Row{ID: runtime.NewUUIDv7(), Title: fmt.Sprint("bulk ", i), Pages: int32(i),
			Price: price(int64(i)), AuthorID: octavia.ID, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			Tags: []string{}, Attrs: runtime.JSON(`{}`)}
	}
	if n, err := book.InsertAll(ctx, db, bulk); err != nil || n != 2500 {
		t.Fatalf("InsertAll: %d %v", n, err)
	}
	seen := 0
	for r, err := range book.New().Where(book.Title.HasPrefix("bulk ")).Limit(5000).Rows(ctx, db) {
		if err != nil {
			t.Fatal(err)
		}
		_ = r
		seen++
	}
	if seen != 2500 {
		t.Fatalf("streamed %d bulk rows", seen)
	}
	t.Log(book.New().Where(book.Pages.Gt(1)).SQL())
}

// The validator is the collection's NOT NULL: a document missing a required
// field is refused by the server, as a typed error.
func TestValidatorRefusesAMissingField(t *testing.T) {
	_, err := db.Exec(context.Background(), `{"insert":"authors","documents":[{"_id":1,"name":"x"}]}`, nil)
	if !errors.Is(err, runtime.ErrCheckViolation) {
		t.Fatalf("got %v", err)
	}
}

// Soft delete: Delete marks, every read and write treats the document as
// gone, the unique email is scoped to the live authors, and Restore and
// HardDelete say the two other things there are to say.
func TestSoftDelete(t *testing.T) {
	ctx := context.Background()
	mk := func(name string) author.Row {
		n := author.Create()
		n.SetName(name)
		n.SetEmail("ghost@example.com")
		r, err := n.Insert(ctx, db)
		if err != nil {
			t.Fatal(name, err)
		}
		return r
	}
	first := mk("First")
	if first.DeletedAt.Valid {
		t.Fatal("a new author is live")
	}
	if err := author.Delete(ctx, db, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := author.Delete(ctx, db, first.ID); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("deleting a deleted author: %v", err)
	}
	q := author.New().Where(author.Email.Eq("ghost@example.com"))
	if n, _ := q.Count(ctx, db); n != 0 {
		t.Fatalf("a deleted author is counted: %d", n)
	}
	if n, _ := q.WithDeleted().Count(ctx, db); n != 1 {
		t.Fatalf("WithDeleted sees %d", n)
	}
	// The address is free again among the live authors…
	second := mk("Second")
	// …but not twice.
	dup := author.Create()
	dup.SetName("Third")
	dup.SetEmail("ghost@example.com")
	if _, err := dup.Insert(ctx, db); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("two live authors with one email: %v", err)
	}
	// A deleted author cannot be updated, and DeleteAll marks live ones only.
	m := author.Mutate(first)
	m.SetName("Zombie")
	if err := m.Update(ctx, db); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("updating a deleted author: %v", err)
	}
	if n, err := q.DeleteAll(ctx, db); err != nil || n != 1 {
		t.Fatalf("DeleteAll: %d %v", n, err)
	}
	if _, err := q.WithDeleted().DeleteAll(ctx, db); err == nil {
		t.Fatal("DeleteAll WithDeleted must be refused")
	}
	if err := author.Restore(ctx, db, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := author.Restore(ctx, db, second.ID); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("restoring a live author: %v", err)
	}
	if err := author.Restore(ctx, db, first.ID); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("restoring into a taken address: %v", err)
	}
	if err := author.HardDelete(ctx, db, first.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.WithDeleted().Count(ctx, db); n != 1 {
		t.Fatalf("after HardDelete, %d remain", n)
	}
}

// Arrays and json are stored as BSON arrays and documents, and the
// predicates give PostgreSQL's answers.
func TestArraysAndJSON(t *testing.T) {
	ctx := context.Background()
	a := author.Create()
	a.SetName("Tagger")
	a.SetEmail("tagger@example.com")
	who, err := a.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	add := func(title string, tags []string, attrs string) book.Row {
		b := book.Create()
		b.SetTitle(title)
		b.SetPages(1)
		b.SetPrice(runtime.Decimal{Unscaled: 1})
		b.SetAuthorID(who.ID)
		if tags != nil {
			b.SetTags(tags)
		}
		if attrs != "" {
			b.SetAttrs(runtime.JSON(attrs))
		}
		r, err := b.Insert(ctx, db)
		if err != nil {
			t.Fatal(title, err)
		}
		return r
	}
	plain := add("plain", nil, "")
	if plain.Tags == nil || len(plain.Tags) != 0 || string(plain.Attrs) != "{}" {
		t.Fatalf("defaults: %#v %s", plain.Tags, plain.Attrs)
	}
	add("sale", []string{"sale", "new"}, `{"format":"hardcover","language":"en","signed":true,"dims":{"h":20,"w":13},"awards":["hugo",{"name":"nebula","year":1975}]}`)
	add("clearance", []string{"clearance"}, `{"format":"paperback","language":["en"]}`)
	mine := book.New().Where(book.AuthorID.Eq(who.ID))
	cases := []struct {
		name string
		p    book.Pred
		want int64
	}{
		{"contains", book.Tags.Contains("sale"), 1},
		{"contains all", book.Tags.Contains("sale", "new"), 1},
		{"contains empty is every array", book.Tags.Contains(), 3},
		{"contained by", book.Tags.ContainedBy("sale", "new", "clearance"), 3},
		{"contained by, narrower", book.Tags.ContainedBy("sale"), 1},
		{"overlaps", book.Tags.Overlaps("new", "clearance"), 2},
		{"json contains", book.Attrs.Contains(runtime.JSON(`{"format":"hardcover"}`)), 1},
		{"json contains nested", book.Attrs.Contains(runtime.JSON(`{"dims":{"h":20}}`)), 1},
		{"json contains element", book.Attrs.Contains(runtime.JSON(`{"awards":["hugo"]}`)), 1},
		{"json contains object element", book.Attrs.Contains(runtime.JSON(`{"awards":[{"name":"nebula"}]}`)), 1},
		{"a scalar is not an array holding it", book.Attrs.Contains(runtime.JSON(`{"language":"en"}`)), 1},
		{"json contains {}", book.Attrs.Contains(runtime.JSON(`{}`)), 3},
		{"has any key", book.Attrs.HasAnyKey("signed", "nope"), 1},
		{"has all keys", book.Attrs.HasAllKeys("format", "language"), 2},
		{"is empty", book.Attrs.IsEmpty(), 1},
	}
	for _, c := range cases {
		n, err := mine.Where(c.p).Count(ctx, db)
		if err != nil || n != c.want {
			t.Errorf("%s: %d, want %d (%v)", c.name, n, c.want, err)
		}
	}
	if _, err := mine.Where(book.Attrs.HasAnyKey("a.b")).Count(ctx, db); err == nil {
		t.Error("a dotted key must be refused, not read as a path")
	}
	m := book.Mutate(plain)
	m.SetTags([]string{"$notapath"})
	m.SetAttrs(runtime.JSON(`{"note":"$x"}`))
	if err := m.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
	if r := m.Row(); len(r.Tags) != 1 || r.Tags[0] != "$notapath" || string(r.Attrs) != `{"note":"$x"}` {
		t.Fatalf("a value that looks like a path is a value: %#v %s", r.Tags, r.Attrs)
	}
}

// A composite key, an optimistic lock and a guarded increment.
func TestCompositeKeyVersionAndIncWhen(t *testing.T) {
	ctx := context.Background()
	b := book.New().Limit(1)
	any, ok, err := b.One(ctx, db)
	if err != nil || !ok {
		t.Fatal("no book", err)
	}
	v := volume.Create()
	v.SetBranch("north")
	v.SetBarcode("0001")
	v.SetBookID(any.ID)
	v.SetOnShelf(true)
	row, err := v.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if row.Branch != "north" || row.Barcode != "0001" || row.Version != 0 {
		t.Fatalf("read-back: %+v", row)
	}
	again := volume.Create()
	again.SetBranch("north")
	again.SetBarcode("0001")
	again.SetBookID(any.ID)
	again.SetOnShelf(true)
	if _, err := again.Insert(ctx, db); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("a duplicate composite key: %v", err)
	}
	other := volume.Row{Branch: "south", Barcode: "0001", BookID: any.ID}
	if err := volume.Insert(ctx, db, &other); err != nil {
		t.Fatal(err)
	}
	if n, _ := volume.New().Where(volume.Barcode.Eq("0001")).Count(ctx, db); n != 2 {
		t.Fatalf("a key column is queryable: %d", n)
	}

	// The guarded increment counts only when the copy was on the shelf, and
	// the version moves with every write.
	m := volume.Mutate(row)
	m.IncCheckoutsWhenOnShelf(true)
	m.SetOnShelf(false)
	if err := m.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
	if r := m.Row(); r.Checkouts != 1 || r.OnShelf || r.Version != 1 {
		t.Fatalf("checkout: %+v", r)
	}
	m.IncCheckoutsWhenOnShelf(true)
	if err := m.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
	if r := m.Row(); r.Checkouts != 1 || r.Version != 2 {
		t.Fatalf("a copy not on the shelf is not checked out again: %+v", r)
	}
	stale := volume.Mutate(row) // read at version 0
	stale.SetOnShelf(true)
	if err := stale.Update(ctx, db); !errors.Is(err, runtime.ErrStaleWrite) {
		t.Fatalf("a stale update: %v", err)
	}
	m.AddCheckouts(10)
	if err := m.Update(ctx, db); err != nil || m.Row().Checkouts != 11 {
		t.Fatalf("Add: %+v %v", m.Row(), err)
	}

	h := hold.Create()
	h.SetBranch("north")
	h.SetBarcode("0001")
	h.SetReader("ana")
	if _, err := h.Insert(ctx, db); err != nil {
		t.Fatal(err)
	}
	if n, err := volume.New().Where(volume.Branch.Eq("south")).UpdateAll(ctx, db, func() volume.Mut {
		var m volume.Mut
		m.IncCheckouts()
		return m
	}()); err != nil || n != 1 {
		t.Fatalf("UpdateAll: %d %v", n, err)
	}
	if err := volume.Delete(ctx, db, "south", "0001"); err != nil {
		t.Fatal(err)
	}
	bulk := []volume.Row{{Branch: "east", Barcode: "1", BookID: any.ID}, {Branch: "east", Barcode: "2", BookID: any.ID}}
	if n, err := volume.InsertAll(ctx, db, bulk); err != nil || n != 2 {
		t.Fatalf("InsertAll with a composite key: %d %v", n, err)
	}
	got, err := volume.New().Where(volume.Branch.Eq("east")).Order(volume.Barcode.Desc()).List(ctx, db)
	if err != nil || len(got) != 2 || got[0].Barcode != "2" {
		t.Fatalf("read back: %+v %v", got, err)
	}
}
