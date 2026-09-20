package msbench

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/schema"
	_ "github.com/microsoft/go-mssqldb"
)

// msStatus is an enum: a Go string type with a declared label set.
//
// SQL Server has no enum TYPE, so it becomes a sized NVARCHAR and a CHECK — and
// for a long time it became nothing at all, because Check accepted a model with
// one and Create then refused it. This is in the gate so that the two halves
// cannot disagree again without a server saying so.
type msStatus string

func (msStatus) EnumValues() []string {
	return []string{"new", "paid", "cancelled"}
}

// The models the gate runs against: a parent with a self-reference, a child
// that soft-deletes, and one column of every type that is supposed to cross.
type msOrg struct {
	storm.Model

	Name    string
	Seats   int32
	Ratio   float64
	Balance storm.Decimal
	Active  bool
	Opened  time.Time
	Note    storm.Null[string]
	Blob    []byte
	Doc     storm.JSON
	Status  msStatus

	Parent  *msOrg
	Members []msMember
	Upper   string
}

func (o *msOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Balance).Numeric(19, 4)
	t.Col(&o.Opened).Date()
	// A PERSISTED computed column, which is the one place SQL Server's DDL
	// differs from MariaDB's in the same direction MySQL's does.
	t.Col(&o.Upper).Generated("UPPER([name])")
	t.Index(&o.Name)
}

// Aggregates: the three grouping-set forms, which is where SQL Server and
// MySQL genuinely part company — ROLLUP, CUBE and GROUPING SETS all exist here
// and only ROLLUP exists there.
func (o *msOrg) Aggregates(a *storm.Aggregates) {
	plain := a.Named("BySeats")
	plain.By(&o.Seats)
	n := plain.Count("Orgs")
	plain.Sum(&o.Balance, "Total")
	plain.Avg(&o.Balance, "Mean")
	plain.Having(a.Gt(n, 0))

	roll := a.Named("Rolled")
	roll.By(&o.Seats)
	roll.By(&o.Active)
	roll.Rollup()
	roll.Count("Orgs")

	cube := a.Named("Cubed")
	cube.By(&o.Seats)
	cube.By(&o.Active)
	cube.Cube()
	cube.Count("Orgs")

	sets := a.Named("Faceted")
	sets.By(&o.Seats)
	sets.By(&o.Active)
	sets.Sets([]string{"Seats"}, []string{"Active"}, nil)
	sets.Count("Orgs")
	sets.GroupingOf("SeatsIsSubtotal", &o.Seats)
}

type msMember struct {
	storm.Model

	Email     string
	Rank      int64
	DeletedAt *time.Time

	Org msOrg
}

// Joins: the plain one and the CTE one, which MySQL refuses outright because
// it cannot lower what the CTE contains.
func (m *msMember) Joins(j *storm.Joins) {
	var o msOrg
	j.Named("WithOrg").
		Inner(&o, &m.Org).
		Take(&m.ID, "MemberID").
		Take(&m.Email, "Email").
		Take(&o.Name, "OrgName").
		Where(j.Gt(&m.Rank, int64(0))).
		OrderDesc(&m.Rank)
}

func (m *msMember) Schema(t *storm.Table) {
	t.Col(&m.Email).Size(255)
	t.Col(&m.Org).OnDelete(storm.Cascade)
	t.SoftDelete(&m.DeletedAt)
	// The unique that MySQL cannot port: live-scoped, so a deleted row keeps
	// nothing reserved. A filtered index is exactly this, and SQL Server has
	// them.
	t.Unique(&m.Email)
}

// A union, so the gate covers the one construct that has no driving table
// (ADR-0008) — and, here, the one whose row cap has to follow every declared
// parameter's ordinal rather than being numbered by a splicer.
//
// One parameter reaching BOTH branches, which is the thing MySQL cannot do:
// bare placeholders bind by position, so the same value would have to be passed
// twice. Named parameters bind once.
var msFeed = storm.Union("Feed", func(u *storm.UnionSpec) {
	org := u.Param("OrgID")

	var o msOrg
	orgs := u.From(&o)
	orgs.Take(&o.CreatedAt, "At")
	orgs.Take(&o.Name, "Text")
	orgs.Const("Kind", "org")
	orgs.Where(storm.Exprs{}.Eq(&o.ID, org))

	var m msMember
	members := u.From(&m)
	members.Take(&m.CreatedAt, "At")
	members.Take(&m.Email, "Text")
	members.Const("Kind", "member")
	members.Where(storm.Exprs{}.Eq(&m.Org, org))

	u.OrderDesc("At")
})

func msSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&msOrg{}, &msMember{}, msFeed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The DDL storm emits for SQL Server has to APPLY, not merely render. A golden
// test proves the generator agrees with itself; only the server can say whether
// the text is SQL Server's.
func TestDDLApplies(t *testing.T) {
	db := open(t)
	s := msSchema(t)

	ddl, err := msddl.Create(s)
	if err != nil {
		t.Fatalf("msddl refused a portable model: %v", err)
	}
	t.Logf("DDL:\n%s", ddl)

	drop(t, db, "ms_members", "ms_orgs")
	for _, stmt := range split(ddl) {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("SQL Server refused\n  %s\n%v", stmt, err)
		}
	}
	t.Cleanup(func() { drop(t, db, "ms_members", "ms_orgs") })
}

// split cuts rendered DDL into statements. msddl separates them with ";\n",
// and nothing it emits contains a semicolon inside a literal.
func split(ddl string) []string {
	var out []string
	for _, s := range strings.Split(ddl, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func drop(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	for _, tb := range tables {
		_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS ["+tb+"]")
	}
}

// open is the gate's connection. Shared with the construct probes, which have
// their own; this one is a *sql.DB because the gate is about the SQL rather
// than about the driver's allocation profile.
func open(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " unset")
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The server starts empty in CI. Everything here runs in whatever database
	// the DSN names — master by default — so this only has to exist for the
	// driver tests next door, which name it explicitly.
	if _, err := db.ExecContext(context.Background(),
		"IF DB_ID('storm') IS NULL CREATE DATABASE storm"); err != nil {
		t.Fatal(err)
	}
	return db
}
