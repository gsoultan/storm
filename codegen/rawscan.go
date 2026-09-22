package codegen

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Raw-query scanner generation — the build-time half of storm.SQL[T].
//
// The generate command PREPAREs each declared statement against the MODEL
// (applied to a scratch schema, so a drifted dev database cannot vouch for a
// query the model would reject), and hands the result descriptor here. This
// file matches it against T and emits the scanner; a mismatch is a generation
// error naming the column and the fix, which is the entire point — the query
// that drifted from its row type fails the build, not the 3am page.

// RawField is one column of a prepared statement's result descriptor, reduced
// to what matching needs so no driver type crosses into codegen.
type RawField struct {
	Name string
	// OID is PostgreSQL's type id, which is what its descriptor reports.
	OID uint32
	// SQLType is the type NAME a back end that has no OIDs reports instead —
	// SQL Server's `system_type_name`, which is "int", "nvarchar(50)",
	// "uniqueidentifier". Empty for PostgreSQL.
	//
	// Two fields rather than one abstraction, because the two carry different
	// information and neither converts to the other: an OID is exact and
	// unparameterised, a type name carries a width and has to be read.
	SQLType string
}

// RawScanner is a resolved scanner ready to emit.
type RawScanner struct {
	TypeImport string // import path of the package declaring the row type
	TypePkg    string // its package name
	TypeName   string // the row type's name
	cols       []rawCol
}

type rawCol struct {
	field    string // Go field name
	kind     kind
	nullable bool // the FIELD chose Null[T]; the descriptor cannot say
}

// Nullable reports whether the named field resolved as Null[T].
func (rs RawScanner) Nullable(field string) bool {
	for _, c := range rs.cols {
		if c.field == field {
			return c.nullable
		}
	}
	return false
}

// dedupeRawScanners collapses queries sharing a row type to one scanner —
// RegisterScanner keys by type, so one is all that can exist — after
// verifying the shared descriptors agree column for column. Scanners decode
// by POSITION: two SELECTs feeding one type in different column orders would
// make one of them silently transpose, so disagreement is an error naming
// the type, not a dedupe.
func dedupeRawScanners(in []RawScanner) ([]RawScanner, error) {
	var out []RawScanner
	seen := map[string]RawScanner{}
	for _, rs := range in {
		key := rs.TypeImport + "." + rs.TypeName
		prev, ok := seen[key]
		if !ok {
			seen[key] = rs
			out = append(out, rs)
			continue
		}
		if len(prev.cols) != len(rs.cols) {
			return nil, fmt.Errorf(
				"storm: row type %s is returned by queries with %d and %d result columns — align the SELECT lists or declare a second row type",
				rs.TypeName, len(prev.cols), len(rs.cols))
		}
		for i := range prev.cols {
			if prev.cols[i] != rs.cols[i] {
				return nil, fmt.Errorf(
					"storm: row type %s is returned by two queries whose result columns disagree at position %d (%s vs %s) — align the SELECT lists or declare a second row type",
					rs.TypeName, i+1, prev.cols[i].field, rs.cols[i].field)
			}
		}
	}
	return out, nil
}

// ResolveRawScanner matches a result descriptor against a row type.
//
// Matching is by NAME, column to exported field (org_name → OrgName), because
// position-matching turns a harmless column reorder into silently transposed
// values of the same type. Every column must land in a field and every field
// must be fed by a column — surplus on either side is an error, since an
// unfed field reads as zero and an unlanded column reads as intended.
func ResolveRawScanner(rt reflect.Type, typeImport string, fields []RawField) (RawScanner, error) {
	return ResolveRawScannerFor(rt, typeImport, fields, DialectPostgres)
}

// ResolveRawScannerFor is ResolveRawScanner for a named back end.
//
// The dialect decides how a result column's TYPE is read — an OID on
// PostgreSQL, a type NAME on SQL Server — and nothing else: once the column is
// a kind, the decoder family already routes by dialect and the emitted scanner
// is the same shape.
func ResolveRawScannerFor(rt reflect.Type, typeImport string, fields []RawField,
	d Dialect) (RawScanner, error) {
	if rt.Kind() != reflect.Struct {
		return RawScanner{}, fmt.Errorf("storm.SQL's type parameter must be a struct, not %s", rt)
	}
	// The qualifier must be the package's declared NAME, not its directory:
	// the two differ in perfectly legal layouts (dir rquery, package
	// authzrquery), and reflect reports the name via String().
	rs := RawScanner{
		TypeImport: typeImport,
		TypePkg:    strings.TrimSuffix(rt.String(), "."+rt.Name()),
		TypeName:   rt.Name(),
	}

	fed := map[string]bool{}
	for i, f := range fields {
		fieldName := exportName(f.Name)
		sf, ok := rt.FieldByName(fieldName)
		if !ok {
			k, tn := fieldKind(d, f)
			_ = k
			return RawScanner{}, fmt.Errorf(
				"result column %d %q (%s) has no field in %s\n  → add `%s %s` or alias the column away",
				i+1, f.Name, tn, rt.Name(), fieldName, goTypeOf(d, f))
		}
		k, tn := fieldKind(d, f)
		if k == kindUnsupported {
			return RawScanner{}, fmt.Errorf(
				"result column %d %q has type %s, which storm cannot decode yet — cast it in the query",
				i+1, f.Name, tn)
		}
		want, nullable := fieldShape(sf.Type)
		if want != goTypeOf(d, f) {
			return RawScanner{}, fmt.Errorf(
				"result column %d %q is %s but %s.%s is %s\n  → change the field to `%s %s`, or cast the column",
				i+1, f.Name, tn, rt.Name(), fieldName, goTypeName(sf.Type), fieldName, goTypeOf(d, f))
		}
		fed[fieldName] = true
		rs.cols = append(rs.cols, rawCol{field: fieldName, kind: k, nullable: nullable})
	}
	for i := 0; i < rt.NumField(); i++ {
		if name := rt.Field(i).Name; !fed[name] {
			return RawScanner{}, fmt.Errorf(
				"%s.%s is fed by no result column — remove the field, or select a column aliased %q",
				rt.Name(), name, snakeOf(name))
		}
	}
	return rs, nil
}

// fieldShape reports the field's element Go type and whether it is Null[T].
// A generic instantiation's reflect.Name carries its type argument —
// "Null[string]", not "Null" — which is why this matches on the prefix.
func fieldShape(t reflect.Type) (string, bool) {
	if t.Kind() == reflect.Struct && strings.HasPrefix(t.Name(), "Null[") &&
		strings.HasPrefix(t.PkgPath(), "github.com/gsoultan/storm") {
		v, _ := t.FieldByName("V")
		return goTypeName(v.Type), true
	}
	return goTypeName(t), false
}

// goTypeName is reflect's name for a type, spelled the way the type tables
// spell it.
//
// reflect renders []byte as "[]uint8" — the same type under its other name —
// while baseGoType renders bytea as "[]byte". Compared as strings they differ,
// so EVERY raw query returning a bytea column was refused, with a message
// asking for the declaration it had just been given:
//
//	totp_secret_enc is bytea but Row.TotpSecretEnc is []uint8
//	  → change the field to `TotpSecretEnc []byte`
//
// Normalising here rather than at the comparison keeps the error message
// right too: it is the same function that renders the "is" half.
func goTypeName(t reflect.Type) string {
	s := t.String()
	if s == "[]uint8" {
		return "[]byte"
	}
	// And the ARRAY of them, which is what a uuid is: reflect says
	// "[16]uint8" where baseGoType says "[16]byte". The slice case above was
	// found and fixed when a bytea column was refused; the array case was the
	// same defect one type over, and it was reachable by every raw query
	// returning a UUID — on PostgreSQL as much as on SQL Server, which is only
	// where it happened to be caught.
	if rest, ok := strings.CutSuffix(s, "]uint8"); ok && strings.HasPrefix(rest, "[") {
		return rest + "]byte"
	}
	return s
}

// oidKind maps a wire type OID to the decoder kind and the SQL type name for
// error messages. Only what a SELECT can return; write-side types never appear
// in a descriptor.
// fieldKind reads a result column's type, whichever way its back end reports
// one.
func fieldKind(d Dialect, f RawField) (kind, string) {
	if d == DialectMSSQL {
		return tdsKind(f.SQLType)
	}
	return oidKind(f.OID)
}

func goTypeOf(d Dialect, f RawField) string {
	if d == DialectMSSQL {
		return tdsGoType(f.SQLType)
	}
	return oidGoType(f.OID)
}

// tdsKind maps a SQL Server type NAME onto a storm kind.
//
// The name rather than a type id, because that is what
// sp_describe_first_result_set reports and what a reader of an error can match
// against their own DDL. It arrives parameterised — "nvarchar(50)",
// "decimal(19,4)" — so the width is stripped: storm's kinds are about DECODING,
// and every nvarchar decodes the same way whatever its declared length.
func tdsKind(name string) (kind, string) {
	base := strings.ToLower(name)
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	switch base {
	case "bit":
		return kindBool, name
	case "tinyint", "smallint":
		return kindInt2, name
	case "int":
		return kindInt4, name
	case "bigint":
		return kindInt8, name
	case "real":
		return kindFloat4, name
	case "float":
		return kindFloat8, name
	case "decimal", "numeric", "money", "smallmoney":
		return kindNumeric, name
	case "char", "varchar", "nchar", "nvarchar", "text", "ntext", "xml", "sysname":
		return kindText, name
	case "binary", "varbinary", "image", "timestamp", "rowversion":
		return kindBytes, name
	case "uniqueidentifier":
		return kindUUID, name
	case "datetimeoffset":
		return kindTimestamptz, name
	case "datetime", "datetime2", "smalldatetime":
		// No offset, so it is an instant read as UTC. storm's timestamp and
		// timestamptz decode through the same function here — msdrv normalises
		// both to one layout — and the DIFFERENCE is what the column means,
		// which is the model's business rather than the scanner's.
		return kindTimestamptz, name
	case "date":
		return kindDate, name
	case "time":
		return kindTimeOfDay, name
	}
	return kindUnsupported, name
}

// tdsGoType is the field a caller should declare for a result column, named in
// the error that says the field is missing or the wrong type.
func tdsGoType(name string) string {
	k, _ := tdsKind(name)
	return goTypeForKind(k)
}

func oidKind(oid uint32) (kind, string) {
	switch oid {
	case 16:
		return kindBool, "bool"
	case 21:
		return kindInt2, "int2"
	case 23:
		return kindInt4, "int4"
	case 20:
		return kindInt8, "int8"
	case 700:
		return kindFloat4, "float4"
	case 701:
		return kindFloat8, "float8"
	case 25, 1043, 19:
		return kindText, "text"
	case 17:
		return kindBytes, "bytea"
	case 2950:
		return kindUUID, "uuid"
	case 1184:
		return kindTimestamptz, "timestamptz"
	case 1082:
		return kindDate, "date"
	case 1186:
		return kindInterval, "interval"
	case 1083:
		return kindTimeOfDay, "time"
	case 1700:
		return kindNumeric, "numeric"
	case 3802:
		return kindJSONB, "jsonb"
	case 869, 650:
		return kindInet, "inet"
	case 1009, 1015:
		return kindTextArray, "text[]"
	case 2951:
		return kindUUIDArray, "uuid[]"
	case 1016:
		return kindInt8Array, "int8[]"
	case 1007:
		return kindInt4Array, "int4[]"
	case 1231:
		return kindDecimalArray, "numeric[]"
	}
	return kindUnsupported, fmt.Sprintf("oid %d", oid)
}

// oidGoType is the Go type a column decodes to, for error messages and
// suggested fixes.
func oidGoType(oid uint32) string {
	k, _ := oidKind(oid)
	return goTypeForKind(k)
}

// goTypeForKind is the Go type a result column of this kind lands in.
//
// It goes through baseGoType — the same function the MODEL path uses — so the
// name a raw query's error tells you to declare is the name the generator would
// have produced for a column of that type. A second table of hand-written names
// would drift, and the drift would be an error message telling a caller to
// write a field that does not match.
func goTypeForKind(k kind) string {
	c := &schema.Column{NotNull: true, Type: schema.Type{}}
	switch k {
	case kindBool:
		c.Type.Name = schema.TypeBool
	case kindInt2:
		c.Type.Name = schema.TypeInt2
	case kindInt4:
		c.Type.Name = schema.TypeInt4
	case kindInt8:
		c.Type.Name = schema.TypeInt8
	case kindFloat4:
		c.Type.Name = schema.TypeFloat4
	case kindFloat8:
		c.Type.Name = schema.TypeFloat8
	case kindText:
		c.Type.Name = schema.TypeText
	case kindBytes:
		c.Type.Name = schema.TypeBytea
	case kindUUID:
		c.Type.Name = schema.TypeUUID
	case kindTimestamptz:
		c.Type.Name = schema.TypeTimestamptz
	case kindDate:
		c.Type.Name = schema.TypeDate
	case kindInterval:
		c.Type.Name = schema.TypeInterval
	case kindTimeOfDay:
		c.Type.Name = schema.TypeTime
	case kindNumeric:
		c.Type.Name = schema.TypeNumeric
	case kindJSONB:
		c.Type.Name = schema.TypeJSONB
	case kindInet:
		c.Type.Name = schema.TypeInet
	case kindTextArray:
		c.Type.Name, c.Type.Array = schema.TypeText, true
	case kindUUIDArray:
		c.Type.Name, c.Type.Array = schema.TypeUUID, true
	case kindInt8Array:
		c.Type.Name, c.Type.Array = schema.TypeInt8, true
	case kindInt4Array:
		c.Type.Name, c.Type.Array = schema.TypeInt4, true
	case kindDecimalArray:
		c.Type.Name, c.Type.Array = schema.TypeNumeric, true
	default:
		return "?"
	}
	return baseGoType(c)
}

// emitRawScanners writes one scanner per row type plus the init that registers
// them.
func (g *gen) emitRawScanners(scanners []RawScanner, statements []string) {
	if len(scanners) == 0 && len(statements) == 0 {
		return
	}
	g.p("// Raw-query scanners, registered by row type. The statements were")
	g.p("// PREPAREd against the model at generate time; these decode their")
	g.p("// results with no reflect and no `any`, exactly like a table scanner.")
	g.p("//")
	g.p("// RegisterStatement is the other half, and it is a security boundary:")
	g.p("// a scanner is keyed by row type, so it would answer for ANY query")
	g.p("// returning that type — including one assembled at run time. Only the")
	g.p("// statements listed here run; see storm.RegisterStatement.")
	g.p("func init() {")
	for _, rs := range scanners {
		g.p("\tstorm.RegisterScanner(scan%s)", rs.TypeName)
	}
	for _, sql := range statements {
		g.p("\tstorm.RegisterStatement(%s)", goStringLit(sql))
	}
	g.p("}")
	g.p("")
	for _, rs := range scanners {
		g.p("func scan%s(rv %s, r *%s.%s, sl *runtime.Slab) error {", rs.TypeName, g.dec.rowsType(), rs.TypePkg, rs.TypeName)
		if rawHasFallible(rs, g.dec) {
			g.p("\tvar decErr error")
		}
		for i, c := range rs.cols {
			col := &schema.Column{Name: c.field, NotNull: !c.nullable}
			col.Type = oidSchemaType(c.kind)
			g.p("\t%s", decodeExprIn(col, i, g.dec))
			if fallibleIn(col, g.dec) {
				g.p("\tif decErr != nil {")
				g.p("\t\treturn decErr")
				g.p("\t}")
			}
		}
		g.p("\treturn nil")
		g.p("}")
		g.p("")
	}
}

func rawHasFallible(rs RawScanner, d decoders) bool {
	for _, c := range rs.cols {
		col := &schema.Column{NotNull: !c.nullable, Type: oidSchemaType(c.kind)}
		if fallibleIn(col, d) {
			return true
		}
	}
	return false
}

// oidSchemaType is the IR type whose goKind round-trips to k, so decodeExpr
// can be reused verbatim.
func oidSchemaType(k kind) schema.Type {
	switch k {
	case kindBool:
		return schema.Type{Name: schema.TypeBool}
	case kindInt2:
		return schema.Type{Name: schema.TypeInt2}
	case kindInt4:
		return schema.Type{Name: schema.TypeInt4}
	case kindInt8:
		return schema.Type{Name: schema.TypeInt8}
	case kindFloat4:
		return schema.Type{Name: schema.TypeFloat4}
	case kindFloat8:
		return schema.Type{Name: schema.TypeFloat8}
	case kindText:
		return schema.Type{Name: schema.TypeText}
	case kindBytes:
		return schema.Type{Name: schema.TypeBytea}
	case kindUUID:
		return schema.Type{Name: schema.TypeUUID}
	case kindTimestamptz:
		return schema.Type{Name: schema.TypeTimestamptz}
	case kindDate:
		return schema.Type{Name: schema.TypeDate}
	case kindInterval:
		return schema.Type{Name: schema.TypeInterval}
	case kindTimeOfDay:
		return schema.Type{Name: schema.TypeTime}
	case kindNumeric:
		return schema.Type{Name: schema.TypeNumeric}
	case kindJSONB:
		return schema.Type{Name: schema.TypeJSONB}
	case kindInet:
		return schema.Type{Name: schema.TypeInet}
	case kindTextArray:
		return schema.Type{Name: schema.TypeText, Array: true}
	case kindUUIDArray:
		return schema.Type{Name: schema.TypeUUID, Array: true}
	case kindInt8Array:
		return schema.Type{Name: schema.TypeInt8, Array: true}
	case kindInt4Array:
		return schema.Type{Name: schema.TypeInt4, Array: true}
	case kindDecimalArray:
		return schema.Type{Name: schema.TypeNumeric, Array: true}
	}
	return schema.Type{}
}

// goStringLit renders a statement as a Go literal.
//
// A raw literal where the statement allows one, because these are read: a
// multi-line query escaped onto one line is a diff nobody checks, and what a
// reviewer needs from this init is to see which statements can run.
func goStringLit(s string) string {
	if !strings.ContainsAny(s, "`\r") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// sortedUnique orders the statements and drops duplicates.
//
// Sorted because generated output is byte-deterministic across runs and
// machines, and the registration order is whatever order the bootstrap
// happened to collect declarations in. Unique because two declarations with
// identical text are one statement to the registry.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	n := 0
	for i, s := range out {
		if i > 0 && s == out[n-1] {
			continue
		}
		out[n] = s
		n++
	}
	return out[:n]
}
