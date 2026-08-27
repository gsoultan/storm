package planspike_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgconn"
)

// hostile is the usual corpus plus the ones that get missed: a lone quote, a
// backslash before a quote (which breaks naive escaping in databases with
// backslash escapes on), a NUL, and a unicode quote that some normalisers fold
// into a real one.
var hostile = []string{
	`' OR '1'='1`,
	`'; DROP TABLE users; --`,
	`" OR ""="`,
	`\' OR 1=1 --`,
	`'||(SELECT version())||'`,
	`%' UNION SELECT NULL,NULL,NULL--`,
	"'\x00; DROP TABLE users; --",
	"₩' OR '1'='1",
	`$1`,
	`$$; DROP TABLE users; $$`,
	strings.Repeat("'", 1000),
	"",
}

// THE SECURITY GATE, and it is structural rather than a filter.
//
// storm does not escape values, sanitise them, or inspect them. It never has to:
// the statement text is assembled from constants chosen at BUILD time and
// placeholders numbered by position, so no value can reach it. This test states
// that as the property it is — THE SQL IS IDENTICAL WHATEVER THE VALUE — which
// is stronger than any corpus of payloads, because it holds for payloads nobody
// thought of.
func TestInjection_SQLIsIndependentOfValues(t *testing.T) {
	benign := user.New().Where(user.Email.Eq("a@b.com")).SQL()

	for _, payload := range hostile {
		if got := user.New().Where(user.Email.Eq(payload)).SQL(); got != benign {
			t.Errorf("a value changed the statement text.\npayload: %q\ngot:  %s\nwant: %s",
				payload, got, benign)
		}
	}

	// The same for every operator that binds a value, and for a list.
	for _, q := range []struct {
		name string
		with func(string) string
	}{
		{"Like", func(v string) string { return user.New().Where(user.Email.Like(v)).SQL() }},
		{"NotEq", func(v string) string { return user.New().Where(user.Email.NotEq(v)).SQL() }},
		{"Gt", func(v string) string { return user.New().Where(user.Email.Gt(v)).SQL() }},
		{"In", func(v string) string { return user.New().Where(user.Email.In(v, v)).SQL() }},
		{"Any", func(v string) string {
			return user.New().Any(user.Email.Eq(v), user.Name.Eq(v)).SQL()
		}},
		{"Order+After", func(v string) string {
			return user.New().Order(user.Email.Asc()).After(user.Row{Email: v}).SQL()
		}},
	} {
		base := q.with("benign")
		for _, payload := range hostile {
			if got := q.with(payload); got != base {
				t.Errorf("%s: a value changed the statement text.\npayload: %q\ngot: %s",
					q.name, payload, got)
			}
		}
	}
}

// The placeholder count is a property of the shape, so it is known at build
// time. If a value could add or remove one, it could add or remove a clause.
func TestInjection_PlaceholderCountIsIndependentOfValues(t *testing.T) {
	count := func(sql string) int { return strings.Count(sql, "$") }
	want := count(user.New().Where(user.Email.Eq("x"), user.Name.Like("y")).SQL())

	for _, payload := range hostile {
		sql := user.New().Where(user.Email.Eq(payload), user.Name.Like(payload)).SQL()
		if got := count(sql); got != want {
			t.Errorf("payload %q changed the placeholder count from %d to %d:\n%s",
				payload, want, got, sql)
		}
	}

	// A list binds to ONE placeholder however long it is — which is what makes
	// the relation batch loader's shape value-independent.
	one := count(user.New().Where(user.Email.In("a")).SQL())
	many := count(user.New().Where(user.Email.In(hostile...)).SQL())
	if one != many {
		t.Errorf("a list of %d changed the placeholder count from %d to %d", len(hostile), one, many)
	}
}

// assertDataError requires that a rejected payload was rejected as DATA.
//
// This is the sharpest assertion in the file. A payload that is too long or is
// invalid UTF-8 SHOULD be refused — that is a column constraint and an encoding
// rule doing their jobs on a value. What must never happen is a SYNTAX error or
// an undefined table, because those mean the payload was parsed as SQL. The
// class of error is the evidence, not the presence of one.
func assertDataError(t *testing.T, payload string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("payload %q: unexpected error: %v", payload, err)
	}
	switch pg.Code {
	case "22001", // string_data_right_truncation — the varchar limit
		"22021", // character_not_in_repertoire — Postgres text cannot hold NUL
		"23505": // unique_violation
		return // the value was treated as data, which is the whole claim
	}
	t.Fatalf("payload %q was rejected as %s (%s), not as data — it reached the statement: %v",
		payload, pg.Code, pg.Message, err)
}

// tryCreate inserts a row, returning ok=false when the database refused the
// value as data.
func tryCreate(t *testing.T, ctx context.Context, ex runtime.Executor, email, name string) (user.Row, bool) {
	t.Helper()
	n := user.Create()
	n.SetEmail(email)
	n.SetName(name)
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))
	r, err := n.Insert(ctx, ex)
	if err != nil {
		assertDataError(t, name, err)
		return user.Row{}, false
	}
	return r, true
}

// The payloads must reach the database as DATA and come back unchanged. A
// filter would mangle them; binding does not.
func TestInjection_PayloadsRoundTripAsData(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	for _, payload := range hostile {
		r, ok := tryCreate(t, ctx, ex, "inj@example.com", payload)
		if !ok {
			continue // refused as data, which assertDataError already checked
		}
		got, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
		if err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		if !ok {
			t.Fatalf("payload %q: row vanished", payload)
		}
		if got.Name != payload {
			t.Errorf("payload %q came back as %q — something rewrote it", payload, got.Name)
		}
		// And it must be findable BY that value, which only works if the
		// comparison saw the same bytes.
		n, err := user.New().Where(user.Name.Eq(payload), user.ID.Eq(r.ID)).Count(ctx, ex)
		if err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		if n != 1 {
			t.Errorf("payload %q: found %d rows by value, want 1", payload, n)
		}
		_ = user.Delete(ctx, ex, r.ID)
	}
}

// The obvious one, stated anyway: after running every payload, the table is
// still there and still has its rows.
func TestInjection_NothingWasDropped(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	before, err := user.New().Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range hostile {
		// Reading BY a payload must never error at all: a comparison has no
		// length limit and no encoding rule to break. If this fails, the value
		// reached the statement.
		if _, err := user.New().Where(user.Name.Eq(payload)).Count(ctx, ex); err != nil {
			assertDataError(t, payload, err)
		}
	}
	after, err := user.New().Count(ctx, ex)
	if err != nil {
		t.Fatalf("the users table did not survive: %v", err)
	}
	if before != after {
		t.Errorf("row count changed from %d to %d while only reading", before, after)
	}
}

// Writes bind too. An UPDATE assembles its SET list from a mask, and the mask
// is a set of columns — never a value.
func TestInjection_WritesBindTheirValues(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "injw@example.com", "before")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	for _, payload := range hostile {
		m := user.Mutate(r)
		m.SetName(payload)
		if err := m.Update(ctx, ex); err != nil {
			assertDataError(t, payload, err)
			continue
		}
		r = m.Row()

		got, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
		if err != nil || !ok {
			t.Fatalf("payload %q: re-read failed: %v ok=%v", payload, err, ok)
		}
		if got.Name != payload {
			t.Errorf("payload %q was written as %q", payload, got.Name)
		}
	}
}
