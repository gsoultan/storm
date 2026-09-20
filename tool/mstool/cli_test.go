package mstool

import (
	"strings"
	"testing"
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
