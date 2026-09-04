package storm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
)

// Each fixture is standalone. Embedding a shared unexported struct would make
// the embedded FIELD unexported, and reflect refuses to hand back a value
// obtained from one.
type badSum struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *badSum) Aggregates(a *storm.Aggregates) {
	a.Named("X").Sum(&m.Name, "Total") // sum(text)
}

type badMinMax struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *badMinMax) Aggregates(a *storm.Aggregates) {
	a.Named("X").Max(&m.ID, "Newest") // max(uuid)
}

type badMinBool struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *badMinBool) Aggregates(a *storm.Aggregates) {
	a.Named("X").Min(&m.Active, "Any") // min(bool)
}

type dupField struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *dupField) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.Count("N")
	b.Sum(&m.Balance, "N")
}

type dupName struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *dupName) Aggregates(a *storm.Aggregates) {
	a.Named("X").Count("N")
	a.Named("X").Count("M")
}

type badIdent struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *badIdent) Aggregates(a *storm.Aggregates) {
	a.Named("X").Count("total count")
}

type dupGroup struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *dupGroup) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Name, &m.Name)
	b.Count("N")
}

// PostgreSQL has no sum(text), no max(uuid) and no min(bool). Left to the
// server these are "function max(uuid) does not exist" — raised from a report
// that may only run at month end, months after the line was written. Build
// time is the only useful moment to say so.
func TestAggregateDeclarationsAreCheckedAtBuildTime(t *testing.T) {
	cases := []struct {
		name  string
		model any
		want  []string
	}{
		{"sum over text", &badSum{}, []string{"sum", "text"}},
		{"max over uuid", &badMinMax{}, []string{"max", "uuid"}},
		{"min over bool", &badMinBool{}, []string{"min", "bool"}},
		{"two outputs share a field", &dupField{}, []string{`"N"`}},
		{"two aggregates share a name", &dupName{}, []string{"declared twice"}},
		{"output is not an identifier", &badIdent{}, []string{"identifier"}},
		{"grouped by a column twice", &dupGroup{}, []string{"twice"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := storm.Build(c.model)
			if err == nil {
				t.Fatal("accepted; PostgreSQL would have refused it at run time")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// The shapes that must be ACCEPTED, so the checks above are not simply
// refusing everything.
func TestValidAggregatesBuild(t *testing.T) {
	m := &validAgg{}
	s, err := storm.Build(m)
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("valid_aggs")
	if tbl == nil {
		t.Fatal("no table")
	}
	if len(tbl.Aggregates) != 2 {
		t.Fatalf("got %d aggregates, want 2", len(tbl.Aggregates))
	}
	if got := tbl.Aggregates[0].Name; got != "ByActive" {
		t.Errorf("first aggregate is %q", got)
	}
	if n := len(tbl.Aggregates[0].By); n != 1 {
		t.Errorf("ByActive groups by %d column(s), want 1", n)
	}
	if n := len(tbl.Aggregates[1].By); n != 0 {
		t.Errorf("Totals groups by %d column(s), want 0", n)
	}
}

type validAgg struct {
	storm.Model
	Name    string
	Balance storm.Decimal
	Age     *int16
	Active  bool
}

func (m *validAgg) Aggregates(a *storm.Aggregates) {
	b := a.Named("ByActive")
	b.By(&m.Active)
	b.Count("N")
	b.CountOf(&m.Age, "WithAge")
	b.Sum(&m.Balance, "Total")
	b.Avg(&m.Age, "AvgAge")
	b.Min(&m.Name, "FirstName")
	b.Max(&m.CreatedAt, "Newest")

	t := a.Named("Totals")
	t.Count("N")
	t.Sum(&m.Balance, "Total")
}

// ---- the grouped-column rule ------------------------------------------------

type winUngrouped struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *winUngrouped) Aggregates(a *storm.Aggregates) {
	// row_number() over an UNGROUPED column, next to GROUP BY status. Looks
	// reasonable; PostgreSQL refuses it at execution.
	b := a.Named("X")
	b.By(&m.Status)
	b.Count("N")
	b.RowNumber("Rank", a.Over().OrderByDesc(&m.Total))
}

type selUngrouped struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *selUngrouped) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.Count("N")
	b.Max(&m.Total, "Biggest")
	b.Having(a.Gt(a.Col(&m.Total), 1))
}

type noOrderRank struct {
	storm.Model
	Status string
}

func (m *noOrderRank) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	b.Count("N")
	b.RowNumber("Rank", a.Over()) // a rank over nothing
}

type badFilter struct {
	storm.Model
	Status string
}

func (m *badFilter) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	// Filtering a WINDOW function. "Filter before any aggregate" is no longer
	// reachable — Filter lives on the handle a declaration returns, so there is
	// nothing to call it on until an output exists — but filtering the wrong
	// KIND of output still is.
	b.RowNumber("Rank", a.Over().OrderByAsc(&m.Status)).Filter(a.Eq(&m.Status, "x"))
}

type groupingNoSets struct {
	storm.Model
	Status string
}

func (m *groupingNoSets) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	b.Count("N")
	b.GroupingOf("Sub", &m.Status)
}

// PostgreSQL raises these at EXECUTION, from a report that may only run at
// month end. Build time is the only useful moment to say so.
func TestGroupedColumnRuleIsCheckedAtBuildTime(t *testing.T) {
	for _, c := range []struct {
		name  string
		model any
		want  []string
	}{
		{"window over an ungrouped column", &winUngrouped{}, []string{"total", "grouping expressions"}},
		{"having on an ungrouped column", &selUngrouped{}, []string{"total"}},
		{"rank over an unordered window", &noOrderRank{}, []string{"ranks by nothing"}},
		{"filter on a window function", &badFilter{}, []string{"Filter applies to an aggregate"}},
		{"GROUPING without grouping sets", &groupingNoSets{}, []string{"subtotal"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := storm.Build(c.model)
			if err == nil {
				t.Fatal("accepted; PostgreSQL would have refused it at run time")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q: %v", w, err)
				}
			}
		})
	}
}

type validWindow struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *validWindow) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	day := b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	n := b.Count("N")
	revenue := b.Sum(&m.Total, "Revenue")
	// Over an AGGREGATE and over the grouping expression: both legal.
	b.RowNumber("Rank", a.Over().OrderByDesc(revenue))
	b.Lag(revenue, "Prev", a.Over().OrderByAsc(day))
	b.Having(a.Gt(n, 0))
}

// The rule must not refuse what PostgreSQL accepts: an expression that appears
// in the GROUP BY is usable whole, even though its arguments alone are not.
func TestGroupedColumnRuleAcceptsValidWindows(t *testing.T) {
	s, err := storm.Build(&validWindow{})
	if err != nil {
		t.Fatalf("refused a valid declaration: %v", err)
	}
	tbl := s.Table("valid_windows")
	if tbl == nil || len(tbl.Aggregates) != 1 {
		t.Fatal("no aggregate")
	}
	if got := len(tbl.Aggregates[0].Terms); got != 4 {
		t.Errorf("got %d terms, want 4", got)
	}
	if tbl.Aggregates[0].Having == nil {
		t.Error("the HAVING was dropped")
	}
}

// exportIdent (declaration side) and codegen.exportName must agree, or a
// grouping column's field name and its generated field name differ.
func TestGroupFieldNameMatchesCodegen(t *testing.T) {
	s, err := storm.Build(&validAgg{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("valid_aggs")
	for _, g := range tbl.Aggregates[0].By {
		if g.As != "Active" {
			t.Errorf("grouping field name is %q, want Active", g.As)
		}
	}
}

// ---- arithmetic, DISTINCT and window frames ---------------------------------

type winOverRow struct {
	storm.Model
	Total     storm.Decimal
	CreatedAt time.Time
}

func (m *winOverRow) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	day := b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	// sum(total) OVER (...) in a GROUPED query: with OVER it is a window
	// function, so its argument is read from the grouped rows and `total` is
	// not there. PostgreSQL refuses it; so must storm.
	b.Sum(&m.Total, "Moving").OverWindow(a.Over().OrderByAsc(day))
}

type frameBackwards struct {
	storm.Model
	Total     storm.Decimal
	CreatedAt time.Time
}

func (m *frameBackwards) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	day := b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	rev := b.Sum(&m.Total, "Rev")
	b.SumOver(rev, "Bad", a.Over().OrderByAsc(day).Rows(a.CurrentRow(), a.Preceding(3)))
}

type frameNoOrder struct {
	storm.Model
	Total     storm.Decimal
	CreatedAt time.Time
}

func (m *frameNoOrder) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	rev := b.Sum(&m.Total, "Rev")
	b.SumOver(rev, "Bad", a.Over().Rows(a.Preceding(3), a.CurrentRow()))
}

type frameRangeOffset struct {
	storm.Model
	Total     storm.Decimal
	CreatedAt time.Time
}

func (m *frameRangeOffset) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	day := b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	rev := b.Sum(&m.Total, "Rev")
	b.SumOver(rev, "Bad", a.Over().OrderByAsc(day).Range(a.Preceding(3), a.CurrentRow()))
}

type distinctWindowed struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *distinctWindowed) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	b.CountDistinct(&m.Total, "N").OverWindow(a.Over().OrderByAsc(&m.Status))
}

type groupByAggregate struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *groupByAggregate) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	n := b.Count("N")
	// GROUP BY runs before aggregation, so an aggregate cannot appear in it.
	b.ByExpr("Ratio", a.Div(n, a.Lit(2)))
}

type textArithmetic struct {
	storm.Model
	Status string
	Total  storm.Decimal
}

func (m *textArithmetic) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	b.By(&m.Status)
	b.Compute("Nonsense", a.Add(&m.Status, a.Lit(1)))
}

// Each of these is SQL PostgreSQL would reject, or a silently wrong answer.
// The declaration is refused where the name of the offending output is still
// known, rather than at the first call.
func TestAggregateRefusesInvalidWindowsAndArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		want  string
	}{
		{"windowed aggregate reads an ungrouped column", &winOverRow{}, "SumOver(handle"},
		{"frame start after its end", &frameBackwards{}, "start is after its end"},
		{"frame with no ordering to count along", &frameNoOrder{}, "needs an ORDER BY"},
		{"RANGE with an offset", &frameRangeOffset{}, "use Rows"},
		{"count(DISTINCT) with a window", &distinctWindowed{}, "cannot take a window"},
		{"GROUP BY an aggregate", &groupByAggregate{}, "Compute"},
		{"arithmetic on text", &textArithmetic{}, "arithmetic wants numbers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storm.Build(tc.model)
			if err == nil {
				t.Fatal("accepted a declaration PostgreSQL will not run")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not mention %q:\n%v", tc.want, err)
			}
		})
	}
}

// The share-of-group query: a per-row value beside the partition's total.
//
// `sum(x) OVER (PARTITION BY p ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED
// FOLLOWING)` needs no ORDER BY — that frame is the whole partition however the
// rows are ordered, so it is exactly as deterministic without one. The first
// version of the frame rule refused it, which sent the most common reason to
// want a frame at all back to raw SQL.
type wholePartitionFrame struct {
	storm.Model
	Total     storm.Decimal
	CreatedAt time.Time
}

func (m *wholePartitionFrame) Aggregates(a *storm.Aggregates) {
	b := a.Named("X")
	day := b.ByExpr("Day", a.DateTrunc("day", &m.CreatedAt))
	rev := b.Sum(&m.Total, "Rev")
	b.SumOver(rev, "DayTotal", a.Over().PartitionBy(day).
		Rows(a.UnboundedPreceding(), a.UnboundedFollowing()))
}

func TestWholePartitionFrameNeedsNoOrdering(t *testing.T) {
	s, err := storm.Build(&wholePartitionFrame{})
	if err != nil {
		t.Fatalf("refused a frame that covers the whole partition: %v", err)
	}
	tbl := s.Table("whole_partition_frames")
	if tbl == nil || len(tbl.Aggregates) != 1 {
		t.Fatal("aggregate did not build")
	}
}
