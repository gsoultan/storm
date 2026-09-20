package mstool

// storm.SQL validated against a real SQL Server.
//
// The escape hatch's safety has ONE source: a server of the target's own kind
// types every declared statement before it reaches the allow-list. This is that
// claim, executed — it cannot be asserted on text, because the whole point is
// what the server says rather than what the generator thinks.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/runtime/msdrv"
)

// rawUser is the model the scratch database is built from.
type rawUser struct {
	storm.Model
	Email string
	Rank  int64
	Score float64
	Ok    bool
}

func (u *rawUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(255)
}

// rawRow is the row type a declared query reads into.
type rawRow struct {
	// [16]byte, not storm.UUID: the resolver names the type the GENERATOR would
	// have produced for a column of this kind, and that is the underlying one.
	ID    [16]byte
	Email string
	Rank  int64
	Score float64
	Ok    bool
	Seen  time.Time
}

func mssqlDSN(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STORM_MSSQL_ADDR")
	if addr == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	return "sqlserver://sa:" + urlEscape(os.Getenv("STORM_MSSQL_PASSWORD")) +
		"@" + addr + "?database=storm&encrypt=disable"
}

func urlEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '!' || c == '@' || c == '#' || c == '%' || c == '/' || c == ':' {
			b.WriteString("%" + hex2(c))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func hex2(c byte) string {
	const h = "0123456789ABCDEF"
	return string([]byte{h[c>>4], h[c&0xF]})
}

// The whole path: a scratch database built from the MODEL, two statements
// described against it, and a scanner resolved from what the server reported.
func TestRawQueriesValidateAgainstSQLServer(t *testing.T) {
	dsn := mssqlDSN(t)
	// The scratch database is created and dropped by the code under test; this
	// only proves a server is actually there, so a skip is a skip and a failure
	// is a failure.
	if _, err := msdrv.ParseDSN(dsn); err != nil {
		t.Fatal(err)
	}

	model, err := storm.Build(&rawUser{})
	if err != nil {
		t.Fatal(err)
	}

	decls := []storm.RawDecl{
		storm.SQL[rawRow](
			"SELECT [id] AS [ID], [email] AS [Email], [rank] AS [Rank], " +
				"[score] AS [Score], [ok] AS [Ok], [created_at] AS [Seen] " +
				"FROM [raw_users] WHERE [rank] > @p1"),
		storm.SQLExec("UPDATE [raw_users] SET [rank] = [rank] + 1 WHERE [id] = @p1"),
	}

	scanners, stmts, err := PrepareRaw(dsn, model, decls, true)
	if err != nil {
		t.Fatalf("validating against the model: %v", err)
	}
	// BOTH statements are pinned, scanner or not: an exec that is not in the
	// allow-list is the hole the query half no longer has.
	if len(stmts) != 2 {
		t.Errorf("%d statement(s) pinned, want 2", len(stmts))
	}
	if len(scanners) != 1 {
		t.Fatalf("%d scanner(s), want 1 — SQLExec has no row type", len(scanners))
	}
}

// A statement whose text disagrees with the server about how many parameters it
// takes is a BUILD error, not a confusing arity failure at the first call.
func TestRawQueryArityIsCheckedAgainstTheServer(t *testing.T) {
	dsn := mssqlDSN(t)
	model, err := storm.Build(&rawUser{})
	if err != nil {
		t.Fatal(err)
	}
	// Two parameters in the text, one of them inside a string literal — so the
	// server sees one and the scanner counts two.
	decls := []storm.RawDecl{
		storm.SQLExec("UPDATE [raw_users] SET [email] = '@p2' WHERE [rank] > @p1"),
	}
	_, _, err = PrepareRaw(dsn, model, decls, true)
	if err == nil {
		t.Fatal("a statement whose arity disagrees with the server was accepted")
	}
	if !strings.Contains(err.Error(), "parameter") {
		t.Errorf("the error does not name the disagreement: %v", err)
	}
}

// A column the row type has no field for is named, with the field to add.
func TestRawQueryNamesAMissingField(t *testing.T) {
	dsn := mssqlDSN(t)
	model, err := storm.Build(&rawUser{})
	if err != nil {
		t.Fatal(err)
	}
	decls := []storm.RawDecl{
		storm.SQL[rawRow]("SELECT [id] AS [ID], [email] AS [Nope] FROM [raw_users]"),
	}
	_, _, err = PrepareRaw(dsn, model, decls, true)
	if err == nil {
		t.Fatal("a column with no field was accepted")
	}
	for _, want := range []string{"Nope", "nvarchar"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}
