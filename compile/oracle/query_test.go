package oracle_test

// The lowering, rendered. What this file CANNOT prove is that any of it runs —
// that is internal/oraclespike's job and it needs a server (P6.7). What it
// pins is the set of decisions that would otherwise be invisible: which
// spelling, which order, and which combinations are refused.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/oracle"
	"github.com/gsoultan/storm/schema"
)

func TestIdentQuotesAndPreservesCase(t *testing.T) {
	if got := oracle.Ident("users"); got != `"users"` {
		t.Errorf(`Ident("users") = %s`, got)
	}
	if got := oracle.Ident(`a"b`); got != `"a""b"` {
		t.Errorf("a quote in a name must be doubled, got %s", got)
	}
}

// `:1` is a legal bind variable here where T-SQL's `@1` is a syntax error, so
// ADR-0010's carrier needs no prefix on this target.
func TestPlaceholderNeedsNoPrefix(t *testing.T) {
	if oracle.Placeholder != ":" {
		t.Errorf("Placeholder = %q, want :", oracle.Placeholder)
	}
}

// Offset before limit, and — unlike SQL Server — no literal zero to satisfy a
// grammar that requires OFFSET.
func TestPagingBindsOffsetFirstAndOmitsAnAbsentOffset(t *testing.T) {
	if !oracle.PagingOffsetFirst {
		t.Error("OFFSET precedes FETCH in the text, so the arguments bind in that order")
	}
	capped := oracle.LimitOffsetSuffix(false)
	if strings.Contains(capped, "OFFSET") {
		t.Errorf("FETCH FIRST parses on its own here; no literal zero is needed: %s", capped)
	}
	paged := oracle.LimitOffsetSuffix(true)
	if strings.Index(paged, "OFFSET") > strings.Index(paged, "FETCH") {
		t.Errorf("offset must come first: %s", paged)
	}
}

// OFFSET/FETCH is not a clause of ORDER BY here, so nothing has to be invented
// to satisfy the grammar — the one place Oracle is simpler than SQL Server.
func TestAnUnorderedCappedReadNeedsNoInventedOrdering(t *testing.T) {
	if oracle.OrderFallback != "" {
		t.Errorf("OrderFallback = %q; Oracle parses a capped read with no ORDER BY",
			oracle.OrderFallback)
	}
}

// Oracle HAS NULLS FIRST/LAST, which neither MySQL nor SQL Server does — and
// its defaults are PostgreSQL's, so the two that already hold are omitted.
func TestNullPlacementIsSpelledOnlyWhenItDiffersFromTheDefault(t *testing.T) {
	got := map[int]string{}
	for d := 0; d < oracle.NDirections; d++ {
		got[d] = oracle.OrderTerm(d, `"c"`)
	}
	want := map[int]string{
		0: `"c"`,                 // ascending: Oracle sorts NULLs last already
		1: `"c" DESC`,            // descending: NULLs first already
		2: `"c" NULLS FIRST`,     // the other ascending arrangement
		3: `"c" DESC NULLS LAST`, // the other descending one
	}
	for d, w := range want {
		if got[d] != w {
			t.Errorf("direction %d = %q, want %q", d, got[d], w)
		}
	}
}

// There is no row constructor for inequality, so keyset pagination expands.
func TestRowComparisonExpands(t *testing.T) {
	if !oracle.RowCmpExpand {
		t.Error("(a,b) > (:1,:2) is ORA-00920; the comparison has to expand")
	}
}

// The two refusals nothing else storm targets makes.
func TestSharedRowLocksAreRefusedByName(t *testing.T) {
	for _, m := range []int{oracle.LockShare, oracle.LockShareNoWait, oracle.LockShareSkipLocked} {
		why := oracle.LockRefused(m)
		if why == "" {
			t.Errorf("lock mode %d was accepted; Oracle has no shared row lock", m)
			continue
		}
		if !strings.Contains(why, "LOCK TABLE") {
			t.Errorf("the refusal must name what Oracle has instead, and why it is not "+
				"the same promise: %s", why)
		}
		if oracle.LockSuffix(m) != "" {
			t.Errorf("a refused mode must emit nothing, got %q", oracle.LockSuffix(m))
		}
	}
	for _, m := range []int{oracle.LockUpdate, oracle.LockUpdateNoWait, oracle.LockUpdateSkipLocked} {
		if oracle.LockRefused(m) != "" {
			t.Errorf("exclusive mode %d must be supported", m)
		}
		if !strings.HasPrefix(oracle.LockSuffix(m), " FOR UPDATE") {
			t.Errorf("mode %d = %q", m, oracle.LockSuffix(m))
		}
	}
}

func TestACappedLockedReadIsRefusedAndSaysWhyRownumIsNotTheAnswer(t *testing.T) {
	why := oracle.LockRefusedCapped
	for _, want := range []string{"ORA-02014", "ROWNUM", "ORDER BY"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal must mention %q: %s", want, why)
		}
	}
}

// One bound document, so the statement's shape does not depend on how many
// values the caller passed — ADR-0010, for the fourth time.
func TestInListIsOneBoundDocument(t *testing.T) {
	a, b := oracle.InFrag(`"id"`, "NUMBER(19)", false)
	got := a + b
	if strings.Count(got, oracle.Placeholder) != 1 {
		t.Errorf("an IN list must bind exactly one value: %s", got)
	}
	if !strings.Contains(got, "JSON_TABLE") {
		t.Errorf("want JSON_TABLE, got %s", got)
	}
	// The declared type is what keeps an index range scan in the plan: left to
	// a default the unpacked value is text, and comparing that to a NUMBER key
	// puts the conversion on the COLUMN side.
	if !strings.Contains(got, "NUMBER(19)") {
		t.Errorf("the unpacked column must carry the key's type: %s", got)
	}
	na, nb := oracle.InFrag(`"id"`, "NUMBER(19)", true)
	if !strings.Contains(na+nb, "NOT IN") {
		t.Errorf("negation must be spelled: %s", na+nb)
	}
}

// A fragment carries exactly one placeholder — its last sigil is what the
// splicer numbers — so a lowering that mentions the bound value twice cannot
// be expressed. HasAllKeys is refused on SQL Server for exactly that reason
// and is expressible here, which is worth pinning.
func TestEveryFragmentBindsAtMostOneValue(t *testing.T) {
	for _, op := range []string{
		"Eq", "NotEq", "Gt", "Gte", "Lt", "Lte", "Like", "ILike",
		"IsNull", "IsNotNull", "EqLower", "HasAnyKey", "HasAllKeys",
	} {
		a, b, ok := oracle.Frag(op, `"c"`)
		if !ok {
			t.Errorf("%s has no lowering", op)
			continue
		}
		if n := strings.Count(a+b, oracle.Placeholder); n > 1 {
			t.Errorf("%s binds %d values; a fragment carries one: %s", op, n, a+b)
		}
	}
}

func TestJSONContainmentIsRefusedByName(t *testing.T) {
	for _, op := range []string{"JSONContains", "JSONContainedBy"} {
		if oracle.Supported(op) {
			t.Errorf("%s has no Oracle predicate and must not be claimed", op)
		}
		if why := oracle.Refused(op); !strings.Contains(why, "JSON_EXISTS") {
			t.Errorf("the refusal must name what Oracle does have: %s", why)
		}
	}
}

// The read path and the DDL cannot disagree about what the dialect supports:
// oraddl refuses the array and network COLUMN types, and their operators are
// absent here for the same reason.
func TestOperatorsForRefusedColumnTypesAreAbsent(t *testing.T) {
	for _, op := range []string{"Overlaps", "ContainsArray", "InetContains"} {
		if oracle.Supported(op) {
			t.Errorf("%s is claimed but its column type is refused by oraddl", op)
		}
	}
}

func TestTheClockIsTheServersOwn(t *testing.T) {
	a, _ := oracle.NowFrag("updated_at")
	// Not CURRENT_TIMESTAMP: that is read in the SESSION's time zone, which is
	// a client setting, so a column's value would depend on who connected.
	if !strings.Contains(a, "SYSTIMESTAMP") {
		t.Errorf("want SYSTIMESTAMP, got %s", a)
	}
}

// A CLOB cannot be an IN-list key: Oracle refuses LOB comparison in most
// predicates, so the unpack has to be VARCHAR2 even though the column is a CLOB.
func TestATextKeyUnpacksAsVarcharNotClob(t *testing.T) {
	c := &schema.Column{Name: "email", Type: schema.Type{Name: schema.TypeText}}
	if got := oracle.ColumnType(c); got != "VARCHAR2(4000)" {
		t.Errorf("a text key must unpack as VARCHAR2, got %s", got)
	}
	n := &schema.Column{Name: "id", Type: schema.Type{Name: schema.TypeInt8}}
	if got := oracle.ColumnType(n); got != "NUMBER(19)" {
		t.Errorf("a key's type must come from oraddl, got %s", got)
	}
}

func TestCountReturnsSomethingWideEnough(t *testing.T) {
	// Oracle's count() is a NUMBER with no 32-bit ceiling, so unlike SQL
	// Server there is no count_big to reach for.
	if !strings.Contains(oracle.CountPrefix("t"), "count(*)") {
		t.Errorf("got %s", oracle.CountPrefix("t"))
	}
}

// An unquoted Oracle identifier must begin with a LETTER, so an internal alias
// spelled `_storm_k` is ORA-00911 — where every other target storm has accepts
// a leading underscore. Found by running the lowering, not by reading it.
func TestInternalAliasesAreQuoted(t *testing.T) {
	for _, op := range []string{"HasAnyKey", "HasAllKeys"} {
		a, b, ok := oracle.Frag(op, `"doc"`)
		if !ok {
			t.Fatalf("%s has no lowering", op)
		}
		got := a + b
		if strings.Contains(got, " _storm") || strings.Contains(got, "(_storm") {
			t.Errorf("%s uses a bare alias starting with an underscore, which is "+
				"ORA-00911 here:\n%s", op, got)
		}
		if !strings.Contains(got, `"_storm_k"`) {
			t.Errorf("%s must quote its alias:\n%s", op, got)
		}
	}
}
