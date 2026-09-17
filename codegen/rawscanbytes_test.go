package codegen

import (
	"reflect"
	"testing"
)

// reflect calls []byte "[]uint8"; the type tables call bytea "[]byte". They
// are the same type, so comparing the two spellings as strings refused EVERY
// raw query returning a bytea column — and the refusal asked for the
// declaration it had just been handed:
//
//	totp_secret_enc is bytea but Row.TotpSecretEnc is []uint8
//	  → change the field to `TotpSecretEnc []byte`
//
// Found by a context whose operators carry a sealed TOTP secret.
func TestByteSliceIsSpeltTheWayTheTypeTablesSpellIt(t *testing.T) {
	type row struct {
		Hash    []byte
		Name    string
		Payload []byte
	}
	rt := reflect.TypeOf(row{})

	for _, f := range []string{"Hash", "Payload"} {
		sf, _ := rt.FieldByName(f)
		got, nullable := fieldShape(sf.Type)
		if got != "[]byte" {
			t.Errorf("fieldShape(%s) = %q, want []byte", f, got)
		}
		if nullable {
			t.Errorf("fieldShape(%s) reported nullable; a slice carries its own nil", f)
		}
	}

	// And nothing else changed spelling on the way past.
	sf, _ := rt.FieldByName("Name")
	if got, _ := fieldShape(sf.Type); got != "string" {
		t.Errorf("fieldShape(Name) = %q, want string", got)
	}
}
