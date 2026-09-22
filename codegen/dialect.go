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
	// DialectOracle targets Oracle 23 and later.
	//
	// SQL-ONLY so far, and the boundary is stated rather than discovered:
	// `storm ddl -dialect oracle` and `storm portable oracle` work, and
	// `storm generate` refuses. compile/oraddl and compile/oracle are complete
	// and both are proven against a live server from internal/oraclespike —
	// the DDL applies and every statement the lowering produces executes.
	//
	// What is missing is a RUNTIME. internal/oraclespike measured go-ora at
	// 26.3 allocations per row through driver.Rows, against 0.09 for storm's
	// own SQL Server client, and the gap is the driver's rather than
	// database/sql's. A generated package needs runtime.Rows.RawValues, and a
	// database/sql driver decodes before storm can see the bytes — so Oracle
	// needs either a native client or a second row shape in the port, and
	// neither is a decision to take at the end of a long change. See
	// internal/oraclespike/README.md.
	DialectOracle
)

func (d Dialect) String() string {
	switch d {
	case DialectMySQL:
		return "mysql"
	case DialectMariaDB:
		return "mariadb"
	case DialectMSSQL:
		return "mssql"
	case DialectOracle:
		return "oracle"
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

	// rowType and accessor are which of runtime.Rows' two shapes this family
	// reads — `[][]byte`/`RawValues` for a family that decodes wire bytes,
	// `[]any`/`Values` for one that reads what a database/sql driver already
	// decoded.
	//
	// Chosen HERE, at generate time, which is what keeps it from being a
	// branch at run time: a generated package calls exactly one of the two and
	// never asks which it has. The zero value is the byte shape, so the three
	// families that predate the second one need no entry.
	rowType  string
	accessor string

	// uuid renders the assignment for a NOT NULL uuid column, or nil for the
	// families whose rows are byte slices and can be copied into the array
	// directly. A hook rather than a rename for the same reason text is: the
	// call SHAPE differs, because `copy(r.F[:], rv[i])` needs rv[i] to be a
	// slice and the value shape's is an interface.
	uuid func(field string, i int) string
}

// rowsType is the scanner's parameter type: `[][]byte` unless the family says
// otherwise.
func (d decoders) rowsType() string {
	if d.rowType != "" {
		return d.rowType
	}
	return "[][]byte"
}

// rowsAccessor is the Rows method a generated read calls.
func (d decoders) rowsAccessor() string {
	if d.accessor != "" {
		return d.accessor
	}
	return "RawValues"
}

func decodersFor(d Dialect, runtimeImport string) decoders {
	if d == DialectOracle {
		// The FOURTH family, and the first that reads no bytes at all.
		//
		// A database/sql driver decodes before storm can see the wire, so a
		// generated package for this target scans runtime.Rows.Values rather
		// than RawValues — the second row shape, chosen here and therefore
		// never a branch at run time. See runtime/valdec, whose every mapping
		// was measured against go-ora rather than assumed.
		return decoders{
			pkg:      "valdec",
			imp:      runtimeImport + "/runtime/valdec",
			rowType:  "[]any",
			accessor: "Values",
			// EMPTY. The SQL Server and MySQL families rename where their
			// bytes genuinely mean something else — DateTimeOffset is not
			// Timestamptz — and here nothing does: the names are the same and
			// only the ARGUMENT differs, which is the whole of what the second
			// row shape changes.
			fn: nil,
			// The temporals and the decimal are fallible, for the reason every
			// other family's are: they read a form a wrong value would
			// misread, and reporting it beats returning a plausible zero.
			fallible: map[kind]bool{
				kindTimestamptz: true, kindDate: true,
				kindTimeOfDay: true, kindNumeric: true,
			},
			// A string is already a string here — the driver allocated it —
			// so the slab would be a second allocation for no lifetime
			// benefit. The byte families need one because their wire buffer
			// is reused underneath a live row.
			text: func(field string, i int) string {
				return fmt.Sprintf("r.%s = valdec.Str(rv[%d])", field, i)
			},
			// A NOT NULL uuid cannot be `copy(r.F[:], rv[i])` here: rv[i] is
			// an `any`, and copy needs a slice. The byte families keep the
			// copy, which is why this is a hook rather than a rename.
			uuid: func(field string, i int) string {
				return fmt.Sprintf("r.%s = valdec.UUID(rv[%d])", field, i)
			},
		}
	}
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
