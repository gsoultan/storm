package oratool

// The refusals that happen before a connection is attempted — the ones an
// adopter meets first, and the only part of this package a machine with no
// Oracle can check. The live half is internal/oraclespike's.

import (
	"context"
	"strings"
	"testing"
)

func TestImportRefusesAnEmptyDSNWithTheFlagToPass(t *testing.T) {
	_, err := ImportModel(context.Background(), "oracle", "", "", "example.com/m")
	if err == nil {
		t.Fatal("import without a DSN must not succeed")
	}
	if !strings.Contains(err.Error(), "oracle://") {
		t.Errorf("the refusal must show the flag to pass: %v", err)
	}
}

// The driver is the ADOPTER'S, blank-imported into the bootstrap rather than
// linked into a prebuilt storm binary — so an unregistered one has to fail
// with something a reader can act on.
func TestAnUnregisteredDriverIsNamed(t *testing.T) {
	_, err := ImportModel(context.Background(), "no-such-driver",
		"oracle://u:p@h:1521/x", "", "example.com/m")
	if err == nil {
		t.Fatal("an unregistered driver must not succeed")
	}
	if !strings.Contains(err.Error(), "no-such-driver") {
		t.Errorf("the error must name the driver: %v", err)
	}
}

// go's own message names neither the package nor where to put it. The driver
// is the adopter's — a prebuilt storm binary cannot link every one — so this
// is the first failure a hand-written tool.Main hits, and it has to be an
// instruction rather than a diagnosis.
func TestTheMissingDriverRefusalIsAnInstruction(t *testing.T) {
	_, err := ImportModel(context.Background(), "no-such-driver",
		"oracle://u:p@h:1521/x", "", "example.com/m")
	if err == nil {
		t.Fatal("an unregistered driver must not succeed")
	}
	for _, want := range []string{
		OracleDriverPath, // which package
		"import _",       // and what to write
		"go get",         // and how to get it
		"tool.Main",      // and where it goes
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q:\n%v", want, err)
		}
	}
}

// An error that is NOT about a missing driver passes through unchanged: a DSN
// that does not parse is a different problem and must not be answered with an
// import instruction.
func TestOtherErrorsAreNotDressedUpAsAMissingDriver(t *testing.T) {
	_, err := ImportModel(context.Background(), "no-such-driver", "", "", "example.com/m")
	if err == nil {
		t.Fatal("an empty DSN must be refused")
	}
	if strings.Contains(err.Error(), "go get") {
		t.Errorf("an empty DSN is not a missing driver:\n%v", err)
	}
}
