package planspike_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
)

func dec(t *testing.T, s string) storm.Decimal {
	t.Helper()
	d, err := storm.ParseDecimal(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Money must survive the round trip EXACTLY. This is the test float64 cannot
// pass: 0.10 has no binary representation, so an ORM that stores money in a
// float is wrong before it does anything else.
func TestDecimal_RoundTripsThroughPostgres(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	for _, s := range []string{
		"0.0000", "0.1000", "0.0100", "-0.0100",
		"1234.5678", "-1234.5678", "99999999999999.9999", // 18 digits: the most a Decimal carries
		"0.3000", // 0.1 + 0.2 territory
	} {
		want := dec(t, s)
		n := user.Create()
		n.SetEmail("money@example.com")
		n.SetName("Money")
		n.SetStatus("pending")
		n.SetOrgID(anOrg(t))
		n.SetBalance(want)

		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if r.Balance.String() != s {
			t.Errorf("inserted %s, RETURNING gave %s", s, r.Balance.String())
		}

		got, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
		if err != nil || !ok {
			t.Fatalf("%s: re-read: %v ok=%v", s, err, ok)
		}
		if got.Balance.String() != s {
			t.Errorf("stored %s, read back %s", s, got.Balance.String())
		}
		_ = user.Delete(ctx, ex, r.ID)
	}
}

// A Decimal binds as a numeric parameter, so comparisons happen in the database
// at full precision rather than after a lossy conversion.
func TestDecimal_ComparesInTheDatabase(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreateWith(t, ctx, ex, "cmp@example.com", dec(t, "100.0000"))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	for _, tc := range []struct {
		name string
		pred user.Pred
		want bool
	}{
		{"equal", user.Balance.Eq(dec(t, "100.0000")), true},
		{"not equal to a near miss", user.Balance.Eq(dec(t, "100.0001")), false},
		{"greater than", user.Balance.Gt(dec(t, "99.9999")), true},
		{"less than", user.Balance.Lt(dec(t, "100.0001")), true},
		{"not less than itself", user.Balance.Lt(dec(t, "100.0000")), false},
	} {
		n, err := user.New().Where(user.ID.Eq(r.ID), tc.pred).Count(ctx, ex)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (n == 1) != tc.want {
			t.Errorf("%s: matched %d rows, want %v", tc.name, n, tc.want)
		}
	}
}

// A nullable numeric distinguishes "no credit limit" from "a limit of zero".
func TestDecimal_NullIsNotZero(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreateWith(t, ctx, ex, "nullcred@example.com", dec(t, "0.0000"))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	if _, ok := r.Credit.Get(); ok {
		t.Error("an unset credit came back as present")
	}

	m := user.Mutate(r)
	m.SetCredit(dec(t, "0.0000"))
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	got, _, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := got.Credit.Get()
	if !ok {
		t.Fatal("a credit set to zero came back absent — NULL and 0 were conflated")
	}
	if v.String() != "0.0000" {
		t.Errorf("credit = %s, want 0.0000", v.String())
	}
}

// jsonb comes back as raw bytes the caller unmarshals into a type it declared.
// The generator cannot know a jsonb column's shape — that is what the column is
// for — so decoding into map[string]any would allocate a map per row for
// callers who wanted a struct.
func TestJSONB_RoundTripsAndUnmarshals(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreateWith(t, ctx, ex, "json@example.com", dec(t, "0.0000"))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	// The default is '{}', which must arrive as valid JSON rather than as the
	// wire format's version byte followed by it.
	var prefs map[string]string
	if err := r.Prefs.Unmarshal(&prefs); err != nil {
		t.Fatalf("unmarshalling the default: %v (raw %q)", err, r.Prefs.String())
	}
	if len(prefs) != 0 {
		t.Errorf("default prefs = %v, want empty", prefs)
	}

	// Round-trip a real document.
	m := user.Mutate(r)
	m.SetPrefs(runtime.JSON(`{"theme":"dark","rows":25}`))
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	got, _, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Theme string `json:"theme"`
		Rows  int    `json:"rows"`
	}
	if err := got.Prefs.Unmarshal(&out); err != nil {
		t.Fatalf("unmarshal: %v (raw %q)", err, got.Prefs.String())
	}
	if out.Theme != "dark" || out.Rows != 25 {
		t.Errorf("prefs round-tripped to %+v", out)
	}
}

// A jsonb column offers NO value predicates. Whole-document equality is almost
// never what a caller means, and content filtering needs ->> and @>, which the
// operator set does not have. Offering Eq would be a trap dressed as a feature.
func TestJSONB_HasNoValuePredicates(t *testing.T) {
	// This is a compile-time property; the assertion is that the fixture
	// generates and this file compiles without user.Prefs.Eq existing.
	// testdata/compilefail/jsonb_equality.go pins it.
}

// A value past 18 significant digits must be an error, not a wrong number.
func TestDecimal_OverflowFromTheDatabaseIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	// credit is unconstrained numeric, so the database will happily store this.
	if _, err := ex.Exec(ctx, `
		INSERT INTO users (id, email, name, status, org_id, balance, credit)
		SELECT gen_random_uuid(), 'huge@example.com', 'Huge', 'pending', id, 0,
		       99999999999999999999999999.99
		FROM orgs LIMIT 1`, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ex.Exec(ctx, `DELETE FROM users WHERE email = 'huge@example.com'`, nil)
	})

	_, _, err := user.New().Where(user.Email.Eq("huge@example.com")).One(ctx, ex)
	if !errors.Is(err, runtime.ErrDecimalRange) {
		t.Errorf("reading a 26-digit numeric returned %v, want ErrDecimalRange", err)
	}
}

func mustCreateWith(t *testing.T, ctx context.Context, ex runtime.Executor, email string, bal storm.Decimal) user.Row {
	t.Helper()
	n := user.Create()
	n.SetEmail(email)
	n.SetName("T")
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))
	n.SetBalance(bal)
	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// An array column reads back as a slice, with SQL NULL and '{}' kept distinct —
// those are different facts and a caller checking len(x)==0 conflates them.
func TestArray_RoundTripsAndDistinguishesNullFromEmpty(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreateWith(t, ctx, ex, "arr@example.com", dec(t, "0.0000"))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	// The column defaults to '{}': empty, and not nil.
	if r.Scopes == nil {
		t.Error("the default '{}' came back as nil, which is how SQL NULL reads")
	}
	if len(r.Scopes) != 0 {
		t.Errorf("default scopes = %v, want empty", r.Scopes)
	}

	if _, err := ex.Exec(ctx, `UPDATE users SET scopes = $2 WHERE id = $1`,
		[]any{r.ID, []string{"read", "write", "admin"}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != 3 || got.Scopes[0] != "read" || got.Scopes[2] != "admin" {
		t.Errorf("scopes = %v, want [read write admin]", got.Scopes)
	}

	// Values with the characters that break a naive text-format parser.
	tricky := []string{`a,b`, `{"x"}`, ``, `back\slash`, `"quoted"`, `NULL`}
	if _, err := ex.Exec(ctx, `UPDATE users SET scopes = $2 WHERE id = $1`,
		[]any{r.ID, tricky}); err != nil {
		t.Fatal(err)
	}
	got, _, err = user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != len(tricky) {
		t.Fatalf("got %d elements, want %d", len(got.Scopes), len(tricky))
	}
	for i := range tricky {
		if got.Scopes[i] != tricky[i] {
			t.Errorf("element %d = %q, want %q", i, got.Scopes[i], tricky[i])
		}
	}
}

// A NULL element cannot be represented by a []T. Decoding it as "" would make
// an absent value and an empty one the same thing — the conflation Null[T]
// exists to prevent everywhere else — so it is an error that says what to do.
func TestArray_NullElementIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreateWith(t, ctx, ex, "arrnull@example.com", dec(t, "0.0000"))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	if _, err := ex.Exec(ctx,
		`UPDATE users SET scopes = ARRAY['a', NULL, 'c']::text[] WHERE id = $1`,
		[]any{r.ID}); err != nil {
		t.Fatal(err)
	}
	_, _, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if !errors.Is(err, runtime.ErrArrayNull) {
		t.Errorf("reading an array with a NULL element returned %v, want ErrArrayNull", err)
	}
}

// An array offers no value predicates: containment and overlap need @> and &&,
// which the operator set does not have, and equality on an array is
// order-sensitive in a way almost nobody means.
// testdata/compilefail/array_equality.go pins it.
