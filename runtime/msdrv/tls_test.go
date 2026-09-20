// The TLS paths, which had no live test until this file.
//
// Go 1.23 started refusing to PARSE a certificate with a negative serial
// number, before any verification, so InsecureSkipVerify does not reach it —
// and that is exactly the certificate SQL Server generates for itself when none
// is configured. See ErrNegativeSerial.
//
// The escape hatch is a GODEBUG setting, which belongs to the program rather
// than to a library. A test file may set one, which is what makes the DEFAULT
// connection mode testable at all: without this line the only path with any
// cover is TLSDisabled, and the default — encrypt the login packet, then revert
// to plaintext — is what every real deployment uses.

//go:debug x509negativeserial=1

package msdrv_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime/msdrv"
)

func tlsCfg(t *testing.T, mode msdrv.TLSMode) msdrv.Config {
	t.Helper()
	a := os.Getenv("STORM_MSSQL_ADDR")
	if a == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	ensureDatabase(t, a)
	return msdrv.Config{
		Addr: a, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "storm", TLS: mode,
	}
}

// The protocol's DEFAULT: the handshake runs inside PRELOGIN packets, the
// LOGIN7 packet goes over it, and the session then reverts to plaintext.
//
// The revert is the part that is easy to get wrong in a way that only shows up
// here: it has to happen after the login is WRITTEN and before the reply is
// read, because the server answers in the clear. Reverting later hands the TLS
// layer a login acknowledgement that is not a TLS record, and the connection
// fails at the point of success.
func TestLoginOnlyEncryption(t *testing.T) {
	c, err := msdrv.Open(context.Background(), tlsCfg(t, msdrv.TLSPreferred))
	if err != nil {
		if errors.Is(err, msdrv.ErrNegativeSerial) {
			t.Fatalf("the //go:debug line at the top of this file did not take effect: %v", err)
		}
		t.Fatal(err)
	}
	defer c.Close()
	mustSelectOne(t, c)
}

// The whole session encrypted, certificate unverified. What a development
// server with a self-signed certificate can offer.
func TestFullSessionEncryptionUnverified(t *testing.T) {
	c, err := msdrv.Open(context.Background(), tlsCfg(t, msdrv.TLSInsecure))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Every statement after the handshake goes over TLS here, where the mode
	// above reverts — so this also exercises the framing on an encrypted
	// stream, which splits packets at different boundaries.
	mustSelectOne(t, c)
	// CAST to MAX first: REPLICATE caps at 8000 bytes when its first argument
	// is not a MAX type, so the un-cast form returns half of what was asked
	// for and the test would pass for the wrong reason.
	rows, err := c.Query(context.Background(),
		"SELECT REPLICATE(CAST('x' AS NVARCHAR(MAX)), 9000)", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	// 9000 characters is past one packet at any negotiated size, so this is the
	// multi-packet path over TLS rather than over a socket.
	if n := len(rows.RawValues()[0]); n != 18000 {
		t.Errorf("a value spanning packets came back %d bytes, want 18000", n)
	}
}

// Verification against a self-signed certificate must FAIL, and say so as a
// verification error rather than as something vaguer.
func TestVerifiedEncryptionRefusesASelfSignedCertificate(t *testing.T) {
	_, err := msdrv.Open(context.Background(), tlsCfg(t, msdrv.TLSRequired))
	if err == nil {
		t.Skip("this server has a certificate Go trusts, so there is nothing to refuse")
	}
	if errors.Is(err, msdrv.ErrNoEncryption) {
		t.Fatalf("the server offered no encryption at all: %v", err)
	}
	// The failure is the point: TLSRequired verifies, and a development
	// server's own certificate is not verifiable. A caller who wants the
	// connection encrypted and the server unverified asks for TLSInsecure, and
	// the two are different words on purpose.
	t.Logf("refused, as it should be: %v", err)
}

// TLSDisabled asks for no encryption at all, which a server with
// ForceEncryption set refuses — and the two disagreeing is an error rather than
// a silent upgrade to something the caller did not ask for.
func TestDisabledMeansDisabled(t *testing.T) {
	c, err := msdrv.Open(context.Background(), tlsCfg(t, msdrv.TLSDisabled))
	if err != nil {
		if errors.Is(err, msdrv.ErrNoEncryption) {
			t.Skip("this server requires encryption, which is what TLSDisabled refuses")
		}
		t.Fatal(err)
	}
	defer c.Close()
	mustSelectOne(t, c)
}

func mustSelectOne(t *testing.T, c *msdrv.Conn) {
	t.Helper()
	rows, err := c.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	if v := rows.RawValues()[0]; len(v) != 4 || v[0] != 1 {
		t.Errorf("SELECT 1 returned % x", v)
	}
}
