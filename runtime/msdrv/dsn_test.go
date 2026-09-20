package msdrv_test

import (
	"testing"

	"github.com/gsoultan/storm/runtime/msdrv"
)

// A connection string is what a command line and a secret hold; a Config is
// what this package takes. Getting the percent-decoding wrong shows up as an
// authentication failure that names the USER, which is the wrong end.
func TestParseDSN(t *testing.T) {
	c, err := msdrv.ParseDSN("sqlserver://sa:Storm%21Passw0rd@db.example:1433?database=storm")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db.example:1433" || c.User != "sa" || c.Database != "storm" {
		t.Errorf("got %+v", c)
	}
	// The password held a percent-encoded '!'. Decoded here or nowhere.
	if c.Password != "Storm!Passw0rd" {
		t.Errorf("password = %q", c.Password)
	}
	// The protocol's own default: the login packet encrypted, nothing promised
	// about the rest.
	if c.TLS != msdrv.TLSPreferred {
		t.Errorf("TLS = %v, want TLSPreferred", c.TLS)
	}

	// The port is optional and 1433 is the default.
	if c, err := msdrv.ParseDSN("sqlserver://sa@db.example"); err != nil ||
		c.Addr != "db.example:1433" {
		t.Errorf("got %+v %v", c, err)
	}

	// A bare host:port, which the test harnesses pass.
	if c, err := msdrv.ParseDSN("127.0.0.1:1433"); err != nil || c.Addr != "127.0.0.1:1433" {
		t.Errorf("got %+v %v", c, err)
	}

	for _, tc := range []struct {
		enc  string
		want msdrv.TLSMode
	}{
		{"disable", msdrv.TLSDisabled},
		{"false", msdrv.TLSPreferred},
		{"true", msdrv.TLSInsecure},
		{"strict", msdrv.TLSRequired},
	} {
		c, err := msdrv.ParseDSN("sqlserver://sa@h?encrypt=" + tc.enc)
		if err != nil {
			t.Fatalf("encrypt=%s: %v", tc.enc, err)
		}
		if c.TLS != tc.want {
			t.Errorf("encrypt=%s gave %v, want %v", tc.enc, c.TLS, tc.want)
		}
	}
}

// Every refusal names what is wrong rather than failing later as something
// else.
func TestParseDSNRefusals(t *testing.T) {
	for _, tc := range []struct{ dsn, mentions string }{
		{"", "empty"},
		{"postgres://u@h/db", "sqlserver://"},
		{"sqlserver://u@:1433", "no host"},
		{"sqlserver://u@h:notaport", "port"},
		{"sqlserver://u@h?encrypt=maybe", "disable, false, true or strict"},
	} {
		_, err := msdrv.ParseDSN(tc.dsn)
		if err == nil {
			t.Errorf("%q was accepted", tc.dsn)
			continue
		}
		if !contains(err.Error(), tc.mentions) {
			t.Errorf("%q: %v does not mention %q", tc.dsn, err, tc.mentions)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
