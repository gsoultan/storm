package model

import (
	"fmt"

	"github.com/gsoultan/storm"
)

// ByName is the shape an injection takes: a declaration built inside a
// function, from a caller's string. It is not a package-level var, so nothing
// discovers it, PREPAREs it or registers it — storm refuses to run it, and
// discovery says so at generate time.
func ByName(name string) *storm.SQLQuery[TopRow] {
	return storm.SQL[TopRow](fmt.Sprintf(`SELECT 1 FROM t WHERE name = '%s'`, name))
}

// Purging is the same mistake with the exec half.
func Purging(table string) error {
	_ = storm.SQLExec("DELETE FROM " + table)
	return nil
}

// Allowed: registration from text fixed in the source, which is what a test
// standing in for the generator writes.
const fixedSQL = `SELECT 1`

func init() { storm.RegisterStatement(fixedSQL) }

// Refused: registering what an expression produces whitelists whatever it
// produces, which is the one thing the registry exists to prevent.
func registerFor(table string) {
	storm.RegisterStatement(fmt.Sprintf(`SELECT * FROM %s`, table))
}
