package mydrv

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
)

// Authentication and TLS.
//
// Two plugins, because the servers disagree about the default:
// mysql_native_password is what MariaDB still ships with, and MySQL 8.4 turned
// it off in favour of caching_sha2_password. A client that speaks only one
// connects to only one.
//
// The security-relevant rule is at the bottom of authSHA256: the full-auth
// exchange sends the password in a recoverable form, and it is refused on a
// plaintext connection unless the caller has said in writing that the socket is
// already private.

// Capability flags, the subset this client asserts.
const (
	capLongPassword  = 1
	capLongFlag      = 1 << 2
	capConnectWithDB = 1 << 3
	capProtocol41    = 1 << 9
	capSSL           = 1 << 11
	capSecureConn    = 1 << 15
	capPluginAuth    = 1 << 19
	capPluginAuthLen = 1 << 21
)

// TLSMode says how far the client will go to avoid sending a password in the
// clear.
type TLSMode int

const (
	// TLSPreferred upgrades to TLS when the server offers it and continues
	// without when it does not, and does NOT verify the server's certificate
	// unless the caller supplies a TLSConfig.
	//
	// It is opportunistic encryption, not authentication: it costs a passive
	// observer the traffic and costs an active one nothing. Verifying here
	// instead would mean the default mode could not connect to a stock MySQL
	// or MariaDB — both ship a self-signed certificate — which would push
	// callers to TLSDisabled and leave them with less. Use TLSRequired to make
	// a claim about WHO answered.
	TLSPreferred TLSMode = iota
	// TLSRequired refuses to connect to a server that does not offer TLS, and
	// verifies its certificate. A self-signed server needs a TLSConfig naming
	// its CA — or InsecureSkipVerify, which the caller then owns in writing.
	TLSRequired
	// TLSDisabled never upgrades. Passwords may still be sent under
	// caching_sha2_password's full-auth exchange, which is refused unless
	// AllowCleartextPasswordOverPlaintext is set.
	TLSDisabled
)

// Config is how a connection is opened.
type Config struct {
	Addr, User, Password, Database string

	TLS TLSMode
	// TLSConfig is used when upgrading, exactly as given. A nil value takes
	// its meaning from TLS: verified under TLSRequired, unverified under
	// TLSPreferred. Supplying one overrides that in both directions.
	TLSConfig *tls.Config

	// AllowCleartextPasswordOverPlaintext permits caching_sha2_password's
	// full-auth exchange on a connection with no TLS.
	//
	// That exchange sends the password in a form the wire reveals: cleartext
	// when the server has no public key configured, and RSA-encrypted
	// otherwise — which protects it from a passive observer and not from
	// anyone who can answer as the server. It is needed the FIRST time an
	// account authenticates, before the server has cached the hash.
	//
	// Off by default, and named at length on purpose: a caller who sets it has
	// said the socket is already private.
	AllowCleartextPasswordOverPlaintext bool

	// MaxConns caps how many connections a Pool opens. Zero means
	// DefaultMaxConns. Ignored by Open, which makes exactly one.
	MaxConns int

	// MaxPreparedStmts caps the per-connection prepared-statement cache. Zero
	// means DefaultMaxPreparedStmts.
	MaxPreparedStmts int
}

// ErrCleartextRefused is why a first connection to a caching_sha2_password
// account can fail on a plaintext socket.
var ErrCleartextRefused = errors.New(
	"mydrv: the server asked for caching_sha2_password full authentication, which sends the " +
		"password in a recoverable form, and this connection has no TLS. Use TLS, or set " +
		"Config.AllowCleartextPasswordOverPlaintext if the socket is already private")

// ErrTLSUnsupported is returned for TLSRequired against a server without it.
var ErrTLSUnsupported = errors.New("mydrv: TLSRequired, but the server does not offer TLS")

// greeting is the server's opening packet.
type greeting struct {
	// connID is the server's thread id, and the only handle a second
	// connection has on this one's running statement — COM_KILL_QUERY takes it.
	connID uint32
	salt   []byte
	plugin string
	caps   uint32
}

func parseGreeting(p []byte) (greeting, error) {
	var g greeting
	if len(p) < 2 || p[0] != 10 {
		return g, fmt.Errorf("mydrv: unsupported handshake protocol %d", p[0])
	}
	i := 1
	for i < len(p) && p[i] != 0 {
		i++
	}
	i++ // server version NUL
	if i+4+8 > len(p) {
		return g, errors.New("mydrv: short handshake")
	}
	g.connID = binary.LittleEndian.Uint32(p[i:])
	i += 4
	g.salt = append(g.salt, p[i:i+8]...)
	i += 8 + 1 // salt part 1, filler
	if i+2 <= len(p) {
		g.caps = uint32(binary.LittleEndian.Uint16(p[i:]))
	}
	i += 2
	if i >= len(p) {
		return g, nil
	}
	i += 1 + 2 // charset, status
	if i+2 <= len(p) {
		g.caps |= uint32(binary.LittleEndian.Uint16(p[i:])) << 16
	}
	i += 2
	saltLen := 0
	if i < len(p) {
		saltLen = int(p[i])
	}
	i += 1 + 10
	// Part two of the salt: saltLen counts both parts and the NUL.
	n := saltLen - 8 - 1
	if n < 12 {
		n = 12
	}
	if i+n <= len(p) {
		g.salt = append(g.salt, p[i:i+n]...)
	}
	i += n + 1
	if i < len(p) {
		end := i
		for end < len(p) && p[end] != 0 {
			end++
		}
		g.plugin = string(p[i:end])
	}
	if g.plugin == "" {
		g.plugin = "mysql_native_password"
	}
	return g, nil
}

// nativePassword is SHA1(pass) XOR SHA1(salt + SHA1(SHA1(pass))).
func nativePassword(pass string, salt []byte) []byte {
	if pass == "" {
		return nil
	}
	h1 := sha1.Sum([]byte(pass))
	h2 := sha1.Sum(h1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(h2[:])
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= h1[i]
	}
	return out
}

// sha256Password is caching_sha2_password's fast-path scramble:
// SHA256(pass) XOR SHA256(SHA256(SHA256(pass)) + salt).
func sha256Password(pass string, salt []byte) []byte {
	if pass == "" {
		return nil
	}
	h1 := sha256.Sum256([]byte(pass))
	h2 := sha256.Sum256(h1[:])
	h := sha256.New()
	h.Write(h2[:])
	h.Write(salt)
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= h1[i]
	}
	return out
}

// xorPassword masks the password with the salt, repeating it. Used before RSA
// encryption so the ciphertext differs per connection even for one password.
func xorPassword(pass string, salt []byte) []byte {
	p := append([]byte(pass), 0)
	out := make([]byte, len(p))
	for i := range p {
		out[i] = p[i] ^ salt[i%len(salt)]
	}
	return out
}

// encryptWithPublicKey RSA-OAEP encrypts the salted password with the key the
// server sent.
func encryptWithPublicKey(pemBytes, plain []byte) ([]byte, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("mydrv: the server's public key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("mydrv: the server's public key is not RSA")
	}
	// SHA-1, not SHA-256. The digest here has nothing to do with the plugin's
	// name: it is OAEP's mask function, and the server decrypts with OpenSSL's
	// RSA_PKCS1_OAEP_PADDING, which is SHA-1. Using SHA-256 encrypts something
	// the server cannot read, and it fails as "Access denied" — indistinguishable
	// from a wrong password.
	return rsa.EncryptOAEP(sha1.New(), rand.Reader, rsaPub, plain, nil)
}

// handshake negotiates capabilities, upgrades to TLS if asked, and
// authenticates with whichever plugin the server named.
func (c *conn) handshake(cfg Config, host string) error {
	p, err := c.readPacket()
	if err != nil {
		return err
	}
	g, err := parseGreeting(p)
	if err != nil {
		return err
	}
	c.id = g.connID

	serverTLS := g.caps&capSSL != 0
	useTLS := false
	switch cfg.TLS {
	case TLSRequired:
		if !serverTLS {
			return ErrTLSUnsupported
		}
		useTLS = true
	case TLSPreferred:
		useTLS = serverTLS
	}

	flags := uint32(capLongPassword | capLongFlag | capProtocol41 |
		capSecureConn | capPluginAuth | capPluginAuthLen)
	if cfg.Database != "" {
		flags |= capConnectWithDB
	}
	if useTLS {
		flags |= capSSL
		// The SSL request is the FIRST 32 bytes of the login packet and
		// nothing else: capabilities, max packet, charset, reserved. The
		// server reads it, both sides upgrade, and the real login packet — the
		// one carrying the username and the scramble — is written inside the
		// tunnel.
		req := make([]byte, 0, 32)
		req = binary.LittleEndian.AppendUint32(req, flags)
		req = binary.LittleEndian.AppendUint32(req, 64<<20)
		req = append(req, 45) // utf8mb4
		req = append(req, make([]byte, 23)...)
		if err := c.writePacket(req); err != nil {
			return err
		}
		tc := cfg.TLSConfig
		if tc == nil {
			tc = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
			// Opportunistic mode does not authenticate the server — see
			// TLSPreferred. Under TLSRequired the zero value verifies, which is
			// what makes that mode worth asking for.
			tc.InsecureSkipVerify = cfg.TLS == TLSPreferred //nolint:gosec // documented mode
		}
		tlsConn := tls.Client(c.c, tc)
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("mydrv: TLS handshake: %w", err)
		}
		c.upgrade(tlsConn)
	}

	var auth []byte
	switch g.plugin {
	case "caching_sha2_password":
		auth = sha256Password(cfg.Password, g.salt)
	default:
		auth = nativePassword(cfg.Password, g.salt)
	}

	body := make([]byte, 0, 128)
	body = binary.LittleEndian.AppendUint32(body, flags)
	body = binary.LittleEndian.AppendUint32(body, 64<<20)
	body = append(body, 45)
	body = append(body, make([]byte, 23)...)
	body = append(body, cfg.User...)
	body = append(body, 0)
	body = append(body, byte(len(auth)))
	body = append(body, auth...)
	if cfg.Database != "" {
		body = append(body, cfg.Database...)
		body = append(body, 0)
	}
	body = append(body, g.plugin...)
	body = append(body, 0)
	if err := c.writePacket(body); err != nil {
		return err
	}
	return c.authResult(cfg, g, useTLS)
}

// authResult drives whatever the server asks for after the login packet.
func (c *conn) authResult(cfg Config, g greeting, tlsOn bool) error {
	for {
		p, err := c.readPacket()
		if err != nil {
			return err
		}
		switch p[0] {
		case 0x00: // OK
			return nil
		case 0xff:
			return fmt.Errorf("mydrv: authentication failed: %w", parseError(p))
		case 0xfe:
			// The server wants a different plugin than the one we guessed.
			name, data := splitAuthSwitch(p[1:])
			var next []byte
			switch name {
			case "caching_sha2_password":
				next = sha256Password(cfg.Password, data)
			case "mysql_native_password":
				next = nativePassword(cfg.Password, data)
			default:
				return fmt.Errorf("mydrv: server asked for auth plugin %q, which this driver does not speak", name)
			}
			g.salt = data
			if err := c.writePacket(next); err != nil {
				return err
			}
		case 0x01: // AuthMoreData
			if len(p) < 2 {
				return errors.New("mydrv: short auth-more-data")
			}
			switch p[1] {
			case 0x03: // fast auth succeeded; an OK follows
				continue
			case 0x04:
				if err := c.sha2FullAuth(cfg, g, tlsOn); err != nil {
					return err
				}
			default:
				return fmt.Errorf("mydrv: unexpected auth-more-data 0x%02x", p[1])
			}
		default:
			return fmt.Errorf("mydrv: unexpected auth packet 0x%02x", p[0])
		}
	}
}

// sha2FullAuth is the exchange caching_sha2_password requires the first time an
// account authenticates, before the server has cached the hash.
//
// Over TLS the password goes as cleartext, which is what the protocol
// specifies and what the tunnel is for. Without TLS it is RSA-encrypted under
// the server's public key — which stops a passive observer and does not stop
// anyone who can answer as the server, so it is refused unless the caller has
// said the socket is private.
func (c *conn) sha2FullAuth(cfg Config, g greeting, tlsOn bool) error {
	if tlsOn {
		pw := append([]byte(cfg.Password), 0)
		return c.writePacket(pw)
	}
	if !cfg.AllowCleartextPasswordOverPlaintext {
		return ErrCleartextRefused
	}
	// Ask for the public key: 0x02 is "send it".
	if err := c.writePacket([]byte{0x02}); err != nil {
		return err
	}
	p, err := c.readPacket()
	if err != nil {
		return err
	}
	if len(p) < 2 || p[0] != 0x01 {
		return errors.New("mydrv: the server did not send its public key")
	}
	enc, err := encryptWithPublicKey(p[1:], xorPassword(cfg.Password, g.salt))
	if err != nil {
		return err
	}
	return c.writePacket(enc)
}

func splitAuthSwitch(p []byte) (name string, data []byte) {
	i := 0
	for i < len(p) && p[i] != 0 {
		i++
	}
	name = string(p[:i])
	if i+1 < len(p) {
		data = p[i+1:]
		// The salt is NUL-terminated.
		if n := len(data); n > 0 && data[n-1] == 0 {
			data = data[:n-1]
		}
	}
	return name, data
}
