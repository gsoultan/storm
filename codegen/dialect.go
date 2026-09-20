package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

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
	// DialectMariaDB targets MariaDB 11.
	//
	// It shares MySQL's WIRE protocol, so it needs no driver of its own — but
	// not its SQL: it gains INSERT … RETURNING and loses LATERAL, GROUPING()
	// and ordered WITH ROLLUP, and spells the shared lock differently. See
	// compile/mariadb.
	DialectMariaDB
	// DialectMSSQL targets SQL Server 2016 and later.
	//
	// The first target whose differences are POSITIONAL rather than absences:
	// OUTPUT sits mid-statement, the row cap is a clause of ORDER BY, and the
	// row lock is a table hint. It also has three things MySQL made storm
	// refuse — filtered indexes, covering indexes and GROUPING SETS — so the
	// seam gained capability in both directions. See compile/mssql.
	DialectMSSQL
)

func (d Dialect) String() string {
	switch d {
	case DialectMySQL:
		return "mysql"
	case DialectMariaDB:
		return "mariadb"
	case DialectMSSQL:
		return "mssql"
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
	// text renders the assignment for a NOT NULL text column, or nil for the
	// families whose strings are already UTF-8 and go through the slab
	// directly. It is a hook rather than a rename because the call SHAPE
	// differs: `sl.Str(rv[i])` takes no decoder at all.
	text func(field string, i int) string
}

func decodersFor(d Dialect, runtimeImport string) decoders {
	if d == DialectMSSQL {
		// The THIRD family. It shares no bytes with either of the others:
		// little-endian like MySQL's, but temporals are hundred-nanosecond
		// ticks beside a day number rather than packed components, and strings
		// are UTF-16.
		return decoders{
			pkg: "msdec",
			imp: runtimeImport + "/runtime/msdec",
			fn: map[string]string{
				// storm's timestamptz is a datetimeoffset here, which is a
				// different decoder from datetime2 — it carries the offset.
				"Timestamptz":     "DateTimeOffset",
				"NullTimestamptz": "NullDateTimeOffset",
				"NumericErr":      "Decimal",
				"TimeOfDayErr":    "TimeOfDay",
			},
			// Every temporal and the decimal are fallible here, for the reason
			// MySQL's are: they read a normalised form that a short value would
			// misread, and reporting it beats returning a plausible zero.
			fallible: map[kind]bool{
				kindTimestamptz: true, kindDate: true,
				kindTimeOfDay: true, kindNumeric: true,
			},
			// Text is UTF-16 on this wire, so the slab cannot simply copy it.
			// Without this a generated scanner reads every string as the
			// interleaved-null bytes of its own UTF-16 form — which compiles,
			// runs, and produces mojibake for every row.
			text: func(field string, i int) string {
				return fmt.Sprintf("r.%s = msdec.Str(rv[%d], sl)", field, i)
			},
		}
	}
	if d == DialectMySQL || d == DialectMariaDB {
		// One decoder family for both: they share the wire, and the decoders
		// are about bytes on it rather than the SQL above it.
		return decoders{
			pkg: "mydec",
			imp: runtimeImport + "/runtime/mydec",
			fn: map[string]string{
				// MySQL's temporal types are packed component-wise and its
				// TIME is a signed duration, so these are genuinely different
				// functions rather than one name over different bytes.
				"Timestamptz": "DateTime",
				"NumericErr":  "Decimal",
				// NOT "Duration": that returns a time.Duration and the field is
				// a runtime.TimeOfDay, so the generated assignment would not
				// compile. mydec.TimeOfDay is the conversion, kept there rather
				// than as a cast the scanner would have to spell for one family.
				"TimeOfDayErr": "TimeOfDay",
				// The NULLABLE spellings, which were missing. nullName builds
				// "Null"+the kind's PostgreSQL decoder name, so the rename has
				// to cover those too — otherwise a nullable timestamp emits
				// mydec.NullTimestamptz, a function this family does not have.
				// Nothing caught it because no MySQL fixture had a nullable
				// temporal column, and a soft-delete model has one on day one.
				"NullTimestamptz": "NullDateTime",
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
			kindInt8Array: true, kindInt4Array: true, kindDecimalArray: true, kindInterval: true,
			kindInet: true, kindTimeOfDay: true, kindTstzRange: true,
		},
	}
}

// supports reports whether this dialect has a decoder for the column at all.
//
// PostgreSQL's arrays, ranges, network types and tsvector have no equivalent on
// either other target — compile/myddl and compile/msddl already refuse them in
// DDL, and this is the same refusal on the read path so the two cannot
// disagree. The list is identical for MySQL and SQL Server, which is a fact
// about PostgreSQL's type system rather than a coincidence.
func (d Dialect) supports(c *schema.Column) bool {
	if d == DialectPostgres {
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
// neutral names resolve to `runtime` whatever the family, because they are not
// decoders: Nullable is generic over one, and the Null[T] it returns is the
// row's field type. Prefixing them with the family package produced calls to
// functions that do not exist — mydec.Nullable — which no assertion on the
// emitted TEXT could catch, and which nothing compiled until a gate did.
var neutralDecoders = map[string]bool{"Nullable": true}

func (d decoders) q(name string) string {
	if neutralDecoders[name] {
		return "runtime." + name
	}
	if n, ok := d.fn[name]; ok {
		return d.pkg + "." + n
	}
	return d.pkg + "." + name
}

// needsRuntime reports whether a generated file must import `runtime` in
// addition to its own decoder family. Always: the Row types, the token stream
// and the executor port all live there, whichever family decodes the bytes.
func (d decoders) needsRuntime() bool { return true }

// family is the import a generated file needs for its decoders, or "" when
// that is `runtime` itself.
func (d decoders) family() string {
	if d.pkg == "runtime" {
		return ""
	}
	return d.imp
}
