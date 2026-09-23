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
