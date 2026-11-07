// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package redisdrv

import (
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Options configures a Client. Only Addr is required.
type Options struct {
	// Addr is host:port. Default "localhost:6379".
	Addr string

	// Username and Password authenticate with AUTH. A Password without a
	// Username is the legacy single-password form, which Redis 6+ treats as
	// the user "default".
	Username string
	Password string

	// DB is the logical database chosen with SELECT after connecting.
	DB int

	// TLS, when non-nil, wraps every connection. ParseURL sets it for a
	// rediss:// URL.
	TLS *tls.Config

	// DialTimeout bounds connecting and the handshake. Default 5s.
	DialTimeout time.Duration

	// IOTimeout bounds one command's round trip when the context has no
	// earlier deadline. Default 3s.
	IOTimeout time.Duration

	// MaxIdle is how many idle connections the client keeps. Default 16.
	MaxIdle int

	// MaxOpen caps connections in use at once; a caller beyond it waits for
	// one, or for its context. Default 64.
	MaxOpen int

	// MaxReplyBytes caps one reply's bulk string, and its array length, so a
	// confused or hostile server cannot make the client allocate without
	// bound. Default 64 MiB.
	MaxReplyBytes int
}

func (o Options) withDefaults() Options {
	if o.Addr == "" {
		o.Addr = "localhost:6379"
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.IOTimeout <= 0 {
		o.IOTimeout = 3 * time.Second
	}
	if o.MaxIdle <= 0 {
		o.MaxIdle = 16
	}
	if o.MaxOpen <= 0 {
		o.MaxOpen = 64
	}
	if o.MaxIdle > o.MaxOpen {
		o.MaxIdle = o.MaxOpen
	}
	if o.MaxReplyBytes <= 0 {
		o.MaxReplyBytes = 64 << 20
	}
	return o
}

// ParseURL reads redis://[[user]:password@]host[:port][/db], and rediss://
// for TLS, verified against the host name.
func ParseURL(raw string) (Options, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Options{}, err
	}
	var o Options
	switch u.Scheme {
	case "redis":
	case "rediss":
		o.TLS = &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	default:
		return Options{}, errors.New("redisdrv: URL scheme must be redis or rediss, not " + strconv.Quote(u.Scheme))
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "6379"
	}
	o.Addr = net.JoinHostPort(host, port)
	if u.User != nil {
		o.Username = u.User.Username()
		o.Password, _ = u.User.Password()
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if o.DB, err = strconv.Atoi(p); err != nil || o.DB < 0 {
			return Options{}, errors.New("redisdrv: URL path must be a database number, not " + strconv.Quote(p))
		}
	}
	return o, nil
}
