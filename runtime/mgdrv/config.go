package mgdrv

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is how to reach one MongoDB server.
//
// ONE server: this client speaks to the address it is given and does not
// discover a topology. Point it at a replica set's primary, or at a mongos.
// Transactions need a replica set or a sharded cluster — a standalone server
// refuses them, and StartTx says so on the first statement rather than here.
type Config struct {
	// Addr is host:port.
	Addr string
	// Database is where every command runs.
	Database string

	// User and Password authenticate with SCRAM-SHA-256 against AuthSource
	// ("admin" when empty). No user means no authentication.
	User, Password, AuthSource string

	// TLS, when non-nil, wraps every connection.
	TLS *tls.Config

	// MaxConns bounds the pool; 0 means 10.
	MaxConns int
	// DialTimeout bounds connecting and the handshake; 0 means 10s.
	DialTimeout time.Duration
	// AppName is reported to the server, where it shows in currentOp and
	// the slow-query log.
	AppName string
}

// ParseURI reads a mongodb:// connection string.
//
// The options it knows are the ones it honours. Any other is an error rather
// than ignored: a readPreference this client cannot follow, quietly dropped,
// is a misconfiguration that looks like a working one.
func ParseURI(uri string) (Config, error) {
	var c Config
	u, err := url.Parse(uri)
	if err != nil {
		return c, err
	}
	if u.Scheme != "mongodb" {
		return c, errors.New("mgdrv: want a mongodb:// URI (mongodb+srv:// needs DNS discovery, " +
			"which this client does not do — resolve it to the primary's address)")
	}
	if strings.Contains(u.Host, ",") {
		return c, errors.New("mgdrv: one host only — this client does not discover a topology; " +
			"give it the primary's address")
	}
	c.Addr = u.Host
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		c.Addr = net.JoinHostPort(c.Addr, "27017")
	}
	c.Database = strings.TrimPrefix(u.Path, "/")
	if c.Database == "" {
		c.Database = "test"
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	var caFile string
	useTLS := false
	for k, vs := range u.Query() {
		v := vs[len(vs)-1]
		switch strings.ToLower(k) {
		case "authsource":
			c.AuthSource = v
		case "authmechanism":
			if v != "SCRAM-SHA-256" {
				return c, errors.New("mgdrv: authMechanism " + v + " is not supported; SCRAM-SHA-256 is")
			}
		case "tls", "ssl":
			useTLS = v == "true"
		case "tlscafile":
			caFile = v
		case "maxpoolsize":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return c, errors.New("mgdrv: maxPoolSize must be a positive integer")
			}
			c.MaxConns = n
		case "connecttimeoutms":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return c, errors.New("mgdrv: connectTimeoutMS must be a positive integer")
			}
			c.DialTimeout = time.Duration(n) * time.Millisecond
		case "appname":
			c.AppName = v
		case "directconnection", "replicaset":
			// Both describe what this client already does: talk to the one
			// address it was given.
		default:
			return c, errors.New("mgdrv: URI option " + k + " is not supported by this client")
		}
	}
	if useTLS || caFile != "" {
		host, _, _ := net.SplitHostPort(c.Addr)
		c.TLS = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return c, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return c, errors.New("mgdrv: tlsCAFile holds no certificate")
			}
			c.TLS.RootCAs = pool
		}
	}
	return c, nil
}
