package mstool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/schema"
)

// The refusals that happen before a connection is attempted. They are the ones
// an adopter meets first — a DSN typed wrong, or not passed at all — and they
// are the only part of this package a machine with no SQL Server can check.

func TestImportRefusesAnEmptyDSNWithTheFlagToPass(t *testing.T) {
	_, err := ImportModel("", "dbo", "example.com/m")
	if err == nil {
		t.Fatal("import without a DSN must not succeed")
	}
	if !strings.Contains(err.Error(), "sqlserver://") {
		t.Errorf("the refusal must show the flag to pass: %v", err)
	}
}

func TestImportRejectsAMalformedDSN(t *testing.T) {
	if _, err := ImportModel("://nonsense", "dbo", "example.com/m"); err == nil {
		t.Fatal("a DSN that does not parse must not reach a dial")
	}
}

func TestDialerRefusesAnEmptyDSN(t *testing.T) {
	_, err := Dialer("")
	if err == nil {
		t.Fatal("a dialer with no DSN must not be handed out")
	}
	if !strings.Contains(err.Error(), "sqlserver://") {
		t.Errorf("the refusal must show the flag to pass: %v", err)
	}
}

func TestDialerRejectsAMalformedDSN(t *testing.T) {
	if _, err := Dialer("://nonsense"); err == nil {
		t.Fatal("a DSN that does not parse must not become a dialer")
	}
}

func TestPrepareRawRefusesAnEmptyDSN(t *testing.T) {
	_, _, err := PrepareRaw("", nil, nil, true)
	if err == nil {
		t.Fatal("validating storm.SQL without a server must not succeed")
	}
	if !strings.Contains(err.Error(), "sqlserver://") {
		t.Errorf("the refusal must show the flag to pass: %v", err)
	}
}

// `storm diff` then `storm verify -pending`, both against a real server.
//
// The migration file this replays is not hand-written: it is the plan the tool
// would have written, taken from ReplayAndDiff with no files to replay. So the
// property under test is the one an adopter actually gets — run diff, commit
// the file, and CI can tell you whether the model moved without one.
func TestVerifyPendingReplaysAndThenHasNothingToSay(t *testing.T) {
	dsn := mssqlDSN(t)
	ctx := context.Background()
	model, err := storm.Build(&rawUser{})
	if err != nil {
		t.Fatal(err)
	}

	// The plan `storm diff` would write, against a scratch database with
	// nothing replayed into it.
	first, err := ReplayAndDiff(ctx, dsn, nil, model)
	if err != nil {
		t.Fatalf("ReplayAndDiff with no migrations: %v", err)
	}
	if first.Empty() {
		t.Fatal("an empty scratch database produced no plan")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "0001_init.up.sql")
	if err := os.WriteFile(file, []byte(first.SQL()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Replayed, the model has nothing left to ask for. This is the whole
	// promise of -pending, and it is also a second proof that a migration
	// storm writes for SQL Server APPLIES: the replay is one batch per file,
	// the way a runner does it.
	second, err := ReplayAndDiff(ctx, dsn, []string{file}, model)
	if err != nil {
		t.Fatalf("ReplayAndDiff after replaying: %v", err)
	}
	if !second.Empty() {
		t.Fatalf("the migration does not carry the model it was written from:\n%s", second.SQL())
	}

	// And a model that moved is reported, with the SQL that would fix it.
	model.Table("raw_users").Columns = append(model.Table("raw_users").Columns,
		&schema.Column{Name: "nickname", Type: schema.Type{Name: schema.TypeVarchar, Size: 20}})
	third, err := ReplayAndDiff(ctx, dsn, []string{file}, model)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third.SQL(), "nickname") {
		t.Errorf("a column with no migration was not reported:\n%s", third.SQL())
	}
}
