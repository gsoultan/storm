// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
)

// PoolConfig is a Pool's connection and its limits.
type PoolConfig struct {
	// Conn is how each connection is opened. When it is nil, DSN is parsed
	// with ParseConfig for it, pool settings included.
	Conn *wire.Config
	DSN  string

	// Database and RuntimeParams, when set, override the connection's: the
	// database to connect to, and settings such as search_path sent at
	// startup.
	Database      string
	RuntimeParams map[string]string

	// MaxConns bounds the connections open at once; zero means 4, or the
	// number of CPUs if that is more, as pgxpool does.
	MaxConns int
	// MaxConnLifetime closes a connection given back after this long open;
	// MaxConnIdleTime one left unused this long. Zero means no limit.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// AcquireTimeout bounds a wait for a connection when all MaxConns are in
	// use, after which the call fails with ErrPoolExhausted. Zero means
	// DefaultAcquireTimeout; negative waits as long as the context allows.
	AcquireTimeout time.Duration

	// StatementCacheCapacity is caduceus's: how many prepared statements each
	// connection keeps on the server. Zero is caduceus's default; negative
	// keeps none, which PgBouncer in transaction pooling mode needs.
	StatementCacheCapacity int
	// Tracer, if set, is told of every statement; see caduceus.QueryTracer.
	Tracer caduceus.QueryTracer
}

// options is the caduceus options each of the pool's connections is opened
// with: storm's result formats, always, and the pool's settings.
func (c PoolConfig) options() caduceus.Options {
	o := Options()
	o.StatementCacheCapacity = c.StatementCacheCapacity
	o.Tracer = c.Tracer
	return o
}

// The URL settings ParseConfig reads for the pool, named as pgxpool names
// them so a URL written for one works with the other. Any other pgxpool
// setting is refused rather than sent to the server as a runtime parameter.
const (
	keyMaxConns = "pool_max_conns"
	keyLifetime = "pool_max_conn_lifetime"
	keyIdleTime = "pool_max_conn_idle_time"
)

// ParseConfig parses a connection string, a URL or key=value pairs, with
// pgxpool's pool_max_conns, pool_max_conn_lifetime and
// pool_max_conn_idle_time. pgxpool's other pool_ settings, such as
// pool_min_conns, are accepted and do nothing: the pool opens connections as
// they are asked for and checks each one as it hands it out.
func ParseConfig(dsn string) (PoolConfig, error) {
	conn, err := wire.ParseConfig(dsn)
	if err != nil {
		return PoolConfig{}, err
	}
	c := PoolConfig{Conn: conn}
	// caduceus reads past pgxpool's settings, so they are read here.
	settings := poolSettings(dsn)
	if v, ok := settings[keyMaxConns]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return PoolConfig{}, fmt.Errorf("caduceusdrv: %s=%q is not a positive number", keyMaxConns, v)
		}
		c.MaxConns = n
	}
	for key, dst := range map[string]*time.Duration{keyLifetime: &c.MaxConnLifetime, keyIdleTime: &c.MaxConnIdleTime} {
		if v, ok := settings[key]; ok {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return PoolConfig{}, fmt.Errorf("caduceusdrv: %s=%q is not a duration", key, v)
			}
			*dst = d
		}
	}
	return c, nil
}

// poolSettings returns the pool_ settings of a connection string, a URL's
// query or key=value pairs, which wire.ParseConfig has already checked.
func poolSettings(dsn string) map[string]string {
	out := map[string]string{}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return out
		}
		for key, vs := range u.Query() {
			if strings.HasPrefix(key, "pool_") && len(vs) > 0 {
				out[key] = vs[len(vs)-1]
			}
		}
		return out
	}
	for rest := strings.TrimSpace(dsn); rest != ""; rest = strings.TrimSpace(rest) {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(rest[:eq])
		rest = strings.TrimLeft(rest[eq+1:], " \t")
		var value strings.Builder
		if strings.HasPrefix(rest, "'") {
			i := 1
			for ; i < len(rest) && rest[i] != '\''; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				value.WriteByte(rest[i])
			}
			rest = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			value.WriteString(rest[:end])
			rest = rest[end:]
		}
		if strings.HasPrefix(key, "pool_") {
			out[key] = value.String()
		}
	}
	return out
}

// resolve fills Conn from DSN when it is nil, and applies Database and
// RuntimeParams to a copy of it.
func (c PoolConfig) resolve() (PoolConfig, error) {
	if c.Conn == nil {
		if c.DSN == "" {
			return c, errors.New("caduceusdrv: PoolConfig has neither Conn nor DSN")
		}
		parsed, err := ParseConfig(c.DSN)
		if err != nil {
			return c, err
		}
		c.Conn = parsed.Conn
		if c.MaxConns == 0 {
			c.MaxConns = parsed.MaxConns
		}
		if c.MaxConnLifetime == 0 {
			c.MaxConnLifetime = parsed.MaxConnLifetime
		}
		if c.MaxConnIdleTime == 0 {
			c.MaxConnIdleTime = parsed.MaxConnIdleTime
		}
	}
	if c.Database != "" || len(c.RuntimeParams) > 0 {
		conn := c.Conn.Copy()
		if c.Database != "" {
			conn.Database = c.Database
		}
		params := make(map[string]string, len(conn.RuntimeParams)+len(c.RuntimeParams))
		for k, v := range conn.RuntimeParams {
			params[k] = v
		}
		for k, v := range c.RuntimeParams {
			params[k] = v
		}
		conn.RuntimeParams = params
		c.Conn = conn
	}
	return c, nil
}

// maxConns is MaxConns, or its default when unset.
// DefaultAcquireTimeout is AcquireTimeout's zero: long enough that a busy
// pool under a burst still serves, short enough that a leak is reported
// rather than hung on.
const DefaultAcquireTimeout = 30 * time.Second

func (c PoolConfig) acquireTimeout() time.Duration {
	switch {
	case c.AcquireTimeout == 0:
		return DefaultAcquireTimeout
	case c.AcquireTimeout < 0:
		return 0
	}
	return c.AcquireTimeout
}

func (c PoolConfig) maxConns() int {
	if c.MaxConns > 0 {
		return c.MaxConns
	}
	return max(4, runtime.NumCPU())
}
