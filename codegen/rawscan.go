package codegen

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/gsoultan/raorm/schema"
)

// Raw-query scanner generation — the build-time half of raorm.SQL[T].
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
	OID  uint32
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
				"raorm: row type %s is returned by queries with %d and %d result columns — align the SELECT lists or declare a second row type",
				rs.TypeName, len(prev.cols), len(rs.cols))
		}
		for i := range prev.cols {
			if prev.cols[i] != rs.cols[i] {
				return nil, fmt.Errorf(
					"raorm: row type %s is returned by two queries whose result columns disagree at position %d (%s vs %s) — align the SELECT lists or declare a second row type",
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
	if rt.Kind() != reflect.Struct {
		return RawScanner{}, fmt.Errorf("raorm.SQL's type parameter must be a struct, not %s", rt)
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
			k, tn := oidKind(f.OID)
			_ = k
			return RawScanner{}, fmt.Errorf(
				"result column %d %q (%s) has no field in %s\n  → add `%s %s` or alias the column away",
				i+1, f.Name, tn, rt.Name(), fieldName, oidGoType(f.OID))
		}
		k, tn := oidKind(f.OID)
		if k == kindUnsupported {
			return RawScanner{}, fmt.Errorf(
				"result column %d %q has type %s, which raorm cannot decode yet — cast it in the query",
				i+1, f.Name, tn)
		}
		want, nullable := fieldShape(sf.Type)
		if want != oidGoType(f.OID) {
			return RawScanner{}, fmt.Errorf(
				"result column %d %q is %s but %s.%s is %s\n  → change the field to `%s %s`, or cast the column",
				i+1, f.Name, tn, rt.Name(), fieldName, sf.Type, fieldName, oidGoType(f.OID))
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
		strings.HasPrefix(t.PkgPath(), "github.com/gsoultan/raorm") {
		v, _ := t.FieldByName("V")
		return v.Type.String(), true
	}
	return t.String(), false
}

// oidKind maps a wire type OID to the decoder kind and the SQL type name for
// error messages. Only what a SELECT can return; write-side types never appear
// in a descriptor.
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
	}
	return kindUnsupported, fmt.Sprintf("oid %d", oid)
}

// oidGoType is the Go type a column decodes to, for error messages and
// suggested fixes.
func oidGoType(oid uint32) string {
	k, _ := oidKind(oid)
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
	default:
		return "?"
	}
	return baseGoType(c)
}

// emitRawScanners writes one scanner per row type plus the init that registers
// them.
func (g *gen) emitRawScanners(scanners []RawScanner) {
	if len(scanners) == 0 {
		return
	}
	g.p("// Raw-query scanners, registered by row type. The statements were")
	g.p("// PREPAREd against the model at generate time; these decode their")
	g.p("// results with no reflect and no `any`, exactly like a table scanner.")
	g.p("func init() {")
	for _, rs := range scanners {
		g.p("\traorm.RegisterScanner(scan%s)", rs.TypeName)
	}
	g.p("}")
	g.p("")
	for _, rs := range scanners {
		g.p("func scan%s(rv [][]byte, r *%s.%s, sl *runtime.Slab) error {", rs.TypeName, rs.TypePkg, rs.TypeName)
		if rawHasFallible(rs) {
			g.p("\tvar decErr error")
		}
		for i, c := range rs.cols {
			col := &schema.Column{Name: c.field, NotNull: !c.nullable}
			col.Type = oidSchemaType(c.kind)
			g.p("\t%s", decodeExpr(col, i))
			if fallibleColumn(col) {
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

func rawHasFallible(rs RawScanner) bool {
	for _, c := range rs.cols {
		col := &schema.Column{NotNull: !c.nullable, Type: oidSchemaType(c.kind)}
		if fallibleColumn(col) {
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
	}
	return schema.Type{}
}
