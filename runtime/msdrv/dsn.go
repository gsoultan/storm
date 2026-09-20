package msdrv

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseDSN reads a connection string into a Config.
//
// The form is the one every SQL Server tool accepts:
//
//	sqlserver://sa:secret@host:1433?database=storm&encrypt=disable
//
// It exists because a Config is what this package takes and a STRING is what a
// command line, an environment variable and a Kubernetes secret hold. Without
// it every caller writes the same twenty lines of splitting, and the one who
// gets the percent-decoding wrong finds out as an authentication failure that
// names the user rather than the password.
//
// encrypt maps onto TLSMode, using the names go-mssqldb established so that a
// DSN that works there works here:
//
//	disable          TLSDisabled   no encryption at all
//	false / off      TLSPreferred  the login packet only — the protocol's default
//	true / on        TLSInsecure   the whole session, certificate unverified
//	strict           TLSRequired   the whole session, certificate verified
//
// The default is TLSPreferred, which is the protocol's own: a login that is
// never sent in the clear, and no promise about the rest.
func ParseDSN(dsn string) (Config, error) {
	var c Config
	if dsn == "" {
		return c, fmt.Errorf("msdrv: empty connection string")
	}
	if !strings.Contains(dsn, "://") {
		// A bare host:port, which is what the test harnesses pass. Accepted so
		// that the two forms do not need two code paths at every call site.
		c.Addr = dsn
		return c, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return c, fmt.Errorf("msdrv: %w", err)
	}
	if u.Scheme != "sqlserver" && u.Scheme != "mssql" {
		return c, fmt.Errorf("msdrv: %q is not a SQL Server connection string — "+
			"the scheme is sqlserver://", dsn)
	}
	host := u.Hostname()
	if host == "" {
		return c, fmt.Errorf("msdrv: the connection string names no host")
	}
	port := u.Port()
	if port == "" {
		port = "1433"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return c, fmt.Errorf("msdrv: %q is not a port", port)
	}
	c.Addr = host + ":" + port
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}

	q := u.Query()
	c.Database = q.Get("database")
	if c.Database == "" {
		// The path form — sqlserver://host/instance is an INSTANCE name, not a
		// database, so the path is deliberately not read as one. Naming the
		// difference here because reading it as a database is the mistake this
		// function exists to stop a caller making twice.
		c.Database = q.Get("initial catalog")
	}
	if n := q.Get("app name"); n != "" {
		c.AppName = n
	}
	if n := q.Get("packet size"); n != "" {
		if v, err := strconv.Atoi(n); err == nil {
			c.PacketSize = v
		}
	}
	switch strings.ToLower(q.Get("encrypt")) {
	case "disable":
		c.TLS = TLSDisabled
	case "false", "off", "0", "":
		c.TLS = TLSPreferred
	case "true", "on", "1":
		c.TLS = TLSInsecure
	case "strict":
		c.TLS = TLSRequired
	default:
		return c, fmt.Errorf("msdrv: encrypt=%q is not one of disable, false, true or strict",
			q.Get("encrypt"))
	}
	return c, nil
}
