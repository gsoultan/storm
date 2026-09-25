// Package oracle reads an Oracle schema into storm's IR.
//
// The on-ramp: `storm import` prints the Go model an existing database implies,
// and `storm diff` compares a model against one. Both need the catalogue in the
// same shape a model builds to.
//
// TWO THINGS DIFFER FROM EVERY OTHER INTROSPECTOR STORM HAS.
//
// IT READS THE VALUE SIDE OF THE PORT. Oracle is the target whose adapter is
// over database/sql, so runtime.Rows.RawValues is nil here and Values carries
// what the driver decoded. See runtime.Rows; this is the only introspector that
// reads that side, and it is why the row reader below takes an `any`.
//
// AND IT FOLDS CASE. Oracle stores an identifier as it was CREATED, and an
// unquoted name is folded UP before it gets there — so a database storm did not
// create says USERS, EMAIL, PK_USERS. PostgreSQL folds DOWN, so its catalogue
// already reads the way a model does; SQL Server preserves what it was given
// and is conventionally lowercase. Oracle is the only one where the ordinary
// case is SHOUTING, and a model generated from it verbatim would declare Go
// fields from names nobody wrote.
//
// So a name that is ALL UPPERCASE is lowered, and a name with any lowercase in
// it is left exactly as it is — because that one can only have come from a
// quoted identifier, which is what storm itself writes. The two cases cannot be
// confused: `USERS` was unquoted, `users` was quoted, and both round-trip.
// Measured before this package was written: internal/oraclespike's
// TestUnquotedIdentifiersFoldUp, which found that `fold_probe` and
// "fold_probe" are two different tables.
package oracle

import (
	"context"
	"fmt"
	"strings"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/valdec"
	"github.com/gsoultan/storm/schema"
)

// Conn is the slice of a driver this package needs. runtime.Executor satisfies
// it, which means a sqldrv.Exec over any database/sql handle does.
type Conn interface {
	Query(ctx context.Context, sql string, args []any) (runtime.Rows, error)
}

// Introspect reads one schema into the IR.
//
// namespace is an Oracle SCHEMA, which is a USER — they are the same thing
// here, unlike everywhere else storm targets. Empty means the connected user's
// own, which is what an application sees.
func Introspect(ctx context.Context, c Conn, namespace string) (*schema.Schema, error) {
	s := &schema.Schema{}
	byName := map[string]*schema.Table{}

	for _, step := range []struct {
		what string
		load func(context.Context, Conn, string, *schema.Schema, map[string]*schema.Table) error
	}{
		{"tables", loadTables},
		{"columns", loadColumns},
		{"constraints", loadKeys},
		{"checks", loadChecks},
		{"indexes", loadIndexes},
		{"foreign keys", loadForeignKeys},
	} {
		if err := step.load(ctx, c, namespace, s, byName); err != nil {
			return nil, fmt.Errorf("%s: %w", step.what, err)
		}
	}
	s.Normalize()
	return s, nil
}

// fold is the case rule. See the package comment: ALL UPPERCASE means the name
// was written unquoted and Oracle shouted it; anything else was quoted and is
// already what somebody typed.
func fold(name string) string {
	if name == "" || strings.ToUpper(name) != name {
		return name
	}
	return strings.ToLower(name)
}

// owner is the WHERE clause's schema predicate, and the empty case is not the
// same query with one term removed: `USER` is the connected user, which is what
// an application's unqualified names resolve against.
func owner(ns string) (string, string) {
	if ns == "" {
		return "owner = USER", ""
	}
	// Unquoted names in the catalogue are stored upper, so a caller who typed
	// `storm` means STORM. A caller who typed `Mixed` means a quoted schema
	// and gets it verbatim.
	return "owner = :1", fold2(ns)
}

// fold2 is fold in reverse, for a name going INTO a catalogue query: what the
// caller typed lowercase was almost certainly created unquoted, so it is stored
// upper.
func fold2(name string) string {
	if strings.ToLower(name) == name {
		return strings.ToUpper(name)
	}
	return name
}

type rowReader struct {
	rows runtime.ValueRows
	v    []any
}

func query(ctx context.Context, c Conn, sql string, args ...any) (*rowReader, error) {
	// Values, not RawValues: this target's adapter is over database/sql and
	// the driver decoded before storm could see the wire.
	rows, err := runtime.AsValueRows(c.Query(ctx, sql, args))
	if err != nil {
		return nil, err
	}
	return &rowReader{rows: rows}, nil
}

func (r *rowReader) next() bool {
	if !r.rows.Next() {
		return false
	}
	r.v = r.rows.Values()
	return true
}

func (r *rowReader) close() error { r.rows.Close(); return r.rows.Err() }

// str decodes a text column, FOLDED. Every string this package reads is a
// NAME — a table, a column, a constraint — except the ones str2 reads, which
// are expressions and must not be touched.
func (r *rowReader) str(i int) string {
	if i >= len(r.v) {
		return ""
	}
	return fold(valdec.Str(r.v[i]))
}

// str2 is str without the folding, for an expression or a definition: a CHECK's
// text and an index's expression are SQL, and lowering them would change what
// they mean — `'PAID'` is not `'paid'`.
func (r *rowReader) str2(i int) string {
	if i >= len(r.v) {
		return ""
	}
	return valdec.Str(r.v[i])
}

func (r *rowReader) int(i int) int64 {
	if i >= len(r.v) {
		return 0
	}
	return valdec.Int8(r.v[i])
}

func (r *rowReader) isNull(i int) bool { return i >= len(r.v) || r.v[i] == nil }
