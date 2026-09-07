package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
)

// String and ParseUUID must be inverses, or an id changes shape on the way
// through and turns up later as a cache miss nobody can explain.
func TestParseUUIDRoundTrips(t *testing.T) {
	for _, s := range []string{
		"00000000-0000-0000-0000-000000000000",
		"813d0aed-928a-5493-fdb0-7feb64833200",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	} {
		u, err := storm.ParseUUID(s)
		if err != nil {
			t.Fatalf("ParseUUID(%q): %v", s, err)
		}
		if got := u.String(); got != s {
			t.Errorf("round trip: %q -> %q", s, got)
		}
	}
}

// Refused, not normalised. A lenient parser makes String(Parse(s)) != s for
// every input it quietly reshapes.
func TestParseUUIDRefusesOtherForms(t *testing.T) {
	for _, s := range []string{
		"",
		"813d0aed928a5493fdb07feb64833200",       // unhyphenated
		"{813d0aed-928a-5493-fdb0-7feb64833200}", // braced
		"urn:uuid:813d0aed-928a-5493-fdb0-7feb64833200", // URN
		"813d0aed-928a-5493-fdb0-7feb6483320",           // short
		"813d0aed-928a-5493-fdb0-7feb648332000",         // long
		"813d0aed_928a-5493-fdb0-7feb64833200",          // wrong separator
		"zzzzzzzz-928a-5493-fdb0-7feb64833200",          // not hex
	} {
		if _, err := storm.ParseUUID(s); err == nil {
			t.Errorf("ParseUUID(%q) was accepted", s)
		}
	}
}

// Case is a spelling, not an identity. A uuid arriving uppercase from a .NET
// or SQL Server caller is the same sixteen bytes, and String canonicalises the
// text on the way out — RFC 4122's own rule.
func TestParseUUIDIsCaseInsensitive(t *testing.T) {
	const lower = "813d0aed-928a-5493-fdb0-7feb64833200"
	u, err := storm.ParseUUID(strings.ToUpper(lower))
	if err != nil {
		t.Fatalf("uppercase refused: %v", err)
	}
	l, err := storm.ParseUUID(lower)
	if err != nil {
		t.Fatal(err)
	}
	if u != l {
		t.Errorf("case changed the identity: %v vs %v", u, l)
	}
	if u.String() != lower {
		t.Errorf("String did not canonicalise: %q", u.String())
	}
}
