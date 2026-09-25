package schemaoracle

// The type map, which is the INVERSE of compile/oraddl's and has to be: a
// model imported from a database and then applied to one must produce the same
// columns, or `storm diff` reports drift that is not there.

import (
	"testing"

	"github.com/gsoultan/storm/schema"
)

func TestTypesRoundTripFromTheCatalogue(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		dataLen, charLen, prec, scale int64
		precNull                      bool
		want                          schema.Type
	}{
		// The integer widths oraddl emits, read back by PRECISION.
		{name: "NUMBER", prec: 5, want: schema.Type{Name: schema.TypeInt2}},
		{name: "NUMBER", prec: 10, want: schema.Type{Name: schema.TypeInt4}},
		{name: "NUMBER", prec: 19, want: schema.Type{Name: schema.TypeInt8}},
		{name: "NUMBER", prec: 19, scale: 4,
			want: schema.Type{Name: schema.TypeNumeric, Precision: 19, Scale: 4}},
		// A bare NUMBER, which is what oraddl emits for an unbounded numeric
		// and which Oracle reports with a NULL precision.
		{name: "NUMBER", precNull: true, want: schema.Type{Name: schema.TypeNumeric}},

		{name: "BOOLEAN", want: schema.Type{Name: schema.TypeBool}},
		{name: "BINARY_FLOAT", want: schema.Type{Name: schema.TypeFloat4}},
		{name: "BINARY_DOUBLE", want: schema.Type{Name: schema.TypeFloat8}},
		{name: "CLOB", want: schema.Type{Name: schema.TypeText}},
		{name: "JSON", want: schema.Type{Name: schema.TypeJSONB}},
		{name: "DATE", want: schema.Type{Name: schema.TypeDate}},
		{name: "TIMESTAMP(6)", want: schema.Type{Name: schema.TypeTimestamp}},
		{name: "TIMESTAMP(6) WITH TIME ZONE", want: schema.Type{Name: schema.TypeTimestamptz}},

		// RAW(16) is storm's uuid and nothing else is: a RAW of any other
		// width is bytes, and importing it as a uuid would give the model a
		// [16]byte field for a column that is not one.
		{name: "RAW", dataLen: 16, want: schema.Type{Name: schema.TypeUUID}},
		{name: "RAW", dataLen: 8, want: schema.Type{Name: schema.TypeBytea}},
		{name: "BLOB", want: schema.Type{Name: schema.TypeBytea}},
	} {
		got := typeOf(tc.name, tc.dataLen, tc.charLen, tc.prec, tc.scale, tc.precNull)
		if got != tc.want {
			t.Errorf("%s(len=%d prec=%d scale=%d) = %+v, want %+v",
				tc.name, tc.dataLen, tc.prec, tc.scale, got, tc.want)
		}
	}
}

// THE width trap. data_length is BYTES and char_length is characters, and a
// VARCHAR2(200 CHAR) in a multi-byte character set reports 800 bytes — so a
// reader that picked data_length would quadruple every imported width. M10
// shipped exactly that defect against SQL Server's max_length.
func TestACharacterWidthIsReadAsCharactersNotBytes(t *testing.T) {
	got := typeOf("VARCHAR2", 800, 200, 0, 0, true)
	if got.Size != 200 {
		t.Errorf("width came back as %d; data_length is BYTES and char_length is not", got.Size)
	}
	// And a column with no character length falls back rather than importing
	// a zero-width one.
	if got := typeOf("VARCHAR2", 40, 0, 0, 0, true); got.Size != 40 {
		t.Errorf("a missing char_length must fall back to data_length, got %d", got.Size)
	}
}

// A type storm does not know is kept as its own name rather than guessed at:
// a model carrying it refuses at generate time WITH the name in the message,
// which beats a silent substitution.
func TestAnUnknownTypeKeepsItsName(t *testing.T) {
	if got := typeOf("SDO_GEOMETRY", 0, 0, 0, 0, true); got.Name != "sdo_geometry" {
		t.Errorf("got %q", got.Name)
	}
}

func TestDeleteRulesMapBack(t *testing.T) {
	for in, want := range map[string]schema.Action{
		"CASCADE":   schema.Cascade,
		"SET NULL":  schema.SetNull,
		"NO ACTION": schema.NoAction,
		"":          schema.NoAction,
	} {
		if got := deleteRule(in); got != want {
			t.Errorf("deleteRule(%q) = %q, want %q", in, got, want)
		}
	}
}
