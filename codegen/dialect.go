package codegen

import "github.com/gsoultan/storm/schema"

// Dialect selects which back end a generated package targets.
//
// A GENERATE-time parameter, never a runtime one. The whole reason storm can be
// multi-dialect without paying for it is that the decision is made here and
// baked into the emitted code: there is no branch on the hot path because there
// is no decision left to make (docs/DIALECTS.md).
type Dialect uint8

const (
	// DialectPostgres is the default and the zero value, so every existing
	// caller keeps the back end it already had.
	DialectPostgres Dialect = iota
	// DialectMySQL targets MySQL 8.
	DialectMySQL
)

func (d Dialect) String() string {
	if d == DialectMySQL {
		return "mysql"
	}
	return "postgres"
}

// decoders names the package and the functions a generated scanner calls.
//
// Two families, not one with a flag, because they share no bytes: MySQL is
// little-endian where PostgreSQL is big-endian, and packs its temporal types
// component-wise rather than as an epoch offset. Pointing a scanner at the
// wrong one produces byte-reversed numbers with no error, for every row — so
// the families have different package paths and the choice is structural
// (ADR-0007).
type decoders struct {
	// pkg is the selector a generated file uses, e.g. "runtime" or "mydec".
	pkg string
	// imp is the import path it needs.
	imp string
	// fn renames a PostgreSQL decoder to this family's spelling. A name absent
	// here is unchanged — Int8 is Int8 in both families, and only the BYTES
	// behind it differ — so the map holds only the genuine differences.
	fn map[string]string
	// fallible marks the kinds whose decode returns an error in this family.
	fallible map[kind]bool
}

func decodersFor(d Dialect, runtimeImport string) decoders {
	if d == DialectMySQL {
		return decoders{
			pkg: "mydec",
			imp: runtimeImport + "/runtime/mydec",
			fn: map[string]string{
				// MySQL's temporal types are packed component-wise and its
				// TIME is a signed duration, so these are genuinely different
				// functions rather than one name over different bytes.
				"Timestamptz":  "DateTime",
				"TimeOfDayErr": "Duration",
				"NumericErr":   "Decimal",
			},
			fallible: map[kind]bool{
				// Every temporal type here reads a leading length and can be
				// handed one that does not match, where the PostgreSQL family
				// reads a fixed width and cannot.
				kindTimestamptz: true,
				kindDate:        true, kindTimeOfDay: true, kindNumeric: true,
			},
		}
	}
	return decoders{
		pkg: "runtime",
		imp: runtimeImport + "/runtime",
		fn:  map[string]string{},
		fallible: map[kind]bool{
			kindNumeric: true, kindTextArray: true, kindUUIDArray: true,
			kindInt8Array: true, kindDecimalArray: true, kindInterval: true,
			kindInet: true, kindTimeOfDay: true, kindTstzRange: true,
		},
	}
}

// supports reports whether this dialect has a decoder for the column at all.
//
// PostgreSQL's arrays, ranges, network types and tsvector have no MySQL
// equivalent — `compile/myddl` already refuses them in DDL, and this is the
// same refusal on the read path so the two cannot disagree.
func (d Dialect) supports(c *schema.Column) bool {
	if d != DialectMySQL {
		return true
	}
	if c.Type.Array {
		return false
	}
	switch c.Type.Name {
	case schema.TypeInterval, schema.TypeInet, schema.TypeCIDR,
		schema.TypeMacaddr, schema.TypeTSVector, schema.TypeTstzRange,
		schema.TypeHstore:
		return false
	}
	return true
}

// q is the qualified call a generated scanner makes for a decoder.
//
// Renames first, then qualifies: `Timestamptz` is `runtime.Timestamptz` for
// PostgreSQL and `mydec.DateTime` for MySQL, because the two do not merely
// disagree about bytes — MySQL packs a datetime component-wise with a leading
// length, so it is a different function taking a different shape.
func (d decoders) q(name string) string {
	if n, ok := d.fn[name]; ok {
		return d.pkg + "." + n
	}
	return d.pkg + "." + name
}
