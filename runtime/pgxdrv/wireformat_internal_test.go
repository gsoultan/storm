package pgxdrv

import "testing"

// The rule, as a table, because it was written the wrong way round once and
// the cost was four failing tests on a schema that had always worked. Refuse
// only what raorm decodes from a fixed binary layout; let everything else
// through, since the text-identical set is open-ended (every enum a user
// declares joins it).
func TestFormatOK(t *testing.T) {
	const (
		text  int16 = 0
		bin   int16 = 1
		enum        = uint32(305966) // a user-declared enum in the fixture schema
		compo       = uint32(412001) // any other user-defined type
	)
	for _, tc := range []struct {
		name   string
		oid    uint32
		format int16
		want   bool
	}{
		{"binary bool", 16, bin, true},
		{"binary uuid", 2950, bin, true},
		{"binary numeric", 1700, bin, true},

		// The silent one: 'f' is 0x66, so runtime.Bool would answer true.
		{"text bool", 16, text, false},
		{"text int8", 20, text, false},
		{"text int2", 21, text, false},
		{"text float8", 701, text, false},
		{"text uuid", 2950, text, false},
		{"text bytea", 17, text, false},
		{"text timestamptz", 1184, text, false},
		{"text date", 1082, text, false},
		{"text interval", 1186, text, false},
		{"text numeric", 1700, text, false},
		{"text inet", 869, text, false},
		{"text text[]", 1009, text, false},
		{"text uuid[]", 2951, text, false},
		{"text int8[]", 1016, text, false},

		// Same bytes either way — pgx sends these as text on a binary
		// connection, so refusing them would refuse nearly every real query.
		{"text text", 25, text, true},
		{"text varchar", 1043, text, true},
		{"text name", 19, text, true},
		{"text jsonb", 3802, text, true},

		// An enum's label IS the value; user-defined types are not guessed at.
		{"text enum", enum, text, true},
		{"text other udt", compo, text, true},
	} {
		if got := formatOK(tc.oid, tc.format); got != tc.want {
			t.Errorf("%s: formatOK(%d, %d) = %v, want %v", tc.name, tc.oid, tc.format, got, tc.want)
		}
	}
}

// A domain gets no free pass: PostgreSQL reports the BASE type's OID in the
// row description, so `CREATE DOMAIN positive AS int8` is checked as int8.
// This is a statement about PostgreSQL that the rule depends on; if it ever
// stops holding, the deny-list approach needs revisiting.
func TestFormatOK_DomainsAreCheckedAsTheirBaseType(t *testing.T) {
	if formatOK(20, 0) {
		t.Fatal("int8 in text format must be refused, domain or not")
	}
}
