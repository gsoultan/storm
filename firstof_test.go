package storm_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
	"github.com/jackc/pgx/v5"
)

type foVisit struct {
	storm.Model
	Node string
	Seq  *int64
	At   time.Time
}

// The first visit of each node, as an execution trail orders it: by time, and
// within one moment by sequence with the unnumbered rows first.
func (v *foVisit) Aggregates(a *storm.Aggregates) {
	b := a.Named("Visits")
	b.By(&v.Node)
	b.Count("N")
	b.FirstOf(&v.Seq, "FirstSeq", a.Over().OrderByAsc(&v.At).OrderByAscNullsFirst(&v.Seq))
	b.FirstOf(&v.Seq, "FirstLate", a.Over().OrderByAsc(&v.At).OrderByAsc(&v.Seq)).
		Filter(a.Gt(&v.Seq, a.Lit(4)))
}

func TestFirstOfLowers(t *testing.T) {
	s, err := storm.Build(&foVisit{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Tables[0]
	agg := tbl.Aggregates[0]
	sql := pgsql.AggregateSelect(tbl.Name, agg) + pgsql.AggregateSuffix(agg)
	for _, want := range []string{
		`(array_agg("seq" ORDER BY "at", "seq" NULLS FIRST))[1] AS "first_seq"`,
		`(array_agg("seq" ORDER BY "at", "seq") FILTER (WHERE `,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("%q missing from %s", want, sql)
		}
	}
	for _, term := range agg.Terms {
		if term.As == "FirstSeq" && (!term.Expr.Nullable || term.Expr.Type.Name != schema.TypeInt8) {
			t.Errorf("a first value is the column's type and nullable: %+v", term.Expr)
		}
	}
	if _, err := mysql.Expr(agg.Terms[1].Expr); err == nil {
		t.Error("MySQL lowered a first value it has no spelling for")
	}
}

func TestFirstOfAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&foVisit{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Tables[0]
	agg := tbl.Aggregates[0]
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, `DROP TABLE IF EXISTS `+tbl.Name+`; CREATE TABLE `+tbl.Name+
		`(node text, seq int8, at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(ctx, "DROP TABLE IF EXISTS "+tbl.Name)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		node string
		seq  *int64
		at   time.Time
	}{
		// a: the earliest moment holds 7 and 4, so 4 — not min(seq), which is 1.
		{"a", ptr(int64(1)), t0.Add(time.Hour)}, {"a", ptr(int64(7)), t0}, {"a", ptr(int64(4)), t0},
		// b: an unnumbered row at the earliest moment comes first.
		{"b", ptr(int64(2)), t0}, {"b", nil, t0},
	} {
		if _, err := c.Exec(ctx, `INSERT INTO `+tbl.Name+` VALUES ($1, $2, $3)`, r.node, r.seq, r.at); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.Query(ctx, pgsql.AggregateSelect(tbl.Name, agg)+pgsql.AggregateSuffix(agg))
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		n                int64
		first, firstLate *int64
	}
	seen := map[string]got{}
	for rows.Next() {
		var node string
		var g got
		if err := rows.Scan(&node, &g.n, &g.first, &g.firstLate); err != nil {
			t.Fatal(err)
		}
		seen[node] = g
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if a := seen["a"]; a.n != 3 || a.first == nil || *a.first != 4 || a.firstLate == nil || *a.firstLate != 7 {
		t.Errorf("a: %d rows, first %v, first past 4 %v; want 3, 4, 7", a.n, deref(a.first), deref(a.firstLate))
	}
	if b := seen["b"]; b.n != 2 || b.first != nil || b.firstLate != nil {
		t.Errorf("b: %d rows, first %v, first past 4 %v; want 2, none, none", b.n, deref(b.first), deref(b.firstLate))
	}
}

func ptr[T any](v T) *T { return &v }

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

type foRefused struct {
	storm.Model
	Node string
	Seq  int64
	Tags []string
}

func TestFirstOfRefuses(t *testing.T) {
	for name, decl := range map[string]func(v *foRefused, a *storm.Aggregates){
		"no ordering": func(v *foRefused, a *storm.Aggregates) {
			a.Named("X").By(&v.Node).FirstOf(&v.Seq, "F", a.Over())
		},
		"a partition": func(v *foRefused, a *storm.Aggregates) {
			a.Named("X").By(&v.Node).FirstOf(&v.Seq, "F", a.Over().PartitionBy(&v.Node).OrderByAsc(&v.Seq))
		},
		"an array": func(v *foRefused, a *storm.Aggregates) {
			a.Named("X").By(&v.Node).FirstOf(&v.Tags, "F", a.Over().OrderByAsc(&v.Seq))
		},
		"a window": func(v *foRefused, a *storm.Aggregates) {
			a.Named("X").By(&v.Node).FirstOf(&v.Seq, "F", a.Over().OrderByAsc(&v.Seq)).
				OverWindow(a.Over().OrderByAsc(&v.Node))
		},
	} {
		foDecl = decl
		if _, err := storm.Build(&foRefused{}); err == nil {
			t.Errorf("a first value with %s built", name)
		}
	}
}

var foDecl func(v *foRefused, a *storm.Aggregates)

func (v *foRefused) Aggregates(a *storm.Aggregates) { foDecl(v, a) }
