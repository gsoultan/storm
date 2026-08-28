package model

import "github.com/gsoultan/storm"

type TopRow struct {
	ID    int64
	Count int64
}

// Top is discovered in the call form.
var Top = storm.SQL[TopRow](`SELECT 1`)

// Purge is discovered in the call form, no type parameter.
var Purge = storm.SQLExec(`DELETE FROM sessions`)

// Declared is discovered from its declared type.
var Declared *storm.SQLQuery[TopRow] = storm.SQL[TopRow](`SELECT 2`)

// unexported cannot be reached from a generated bootstrap.
var unexported = storm.SQLExec(`DELETE FROM nothing`)
