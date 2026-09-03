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
