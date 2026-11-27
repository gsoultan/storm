// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/caduceus/wire"
)

// PoolConfig is a Pool's connection and its limits.
type PoolConfig struct {
	// Conn is how each connection is opened.
	Conn *wire.Config

	// MaxConns bounds the connections open at once; zero means 4, or the
	// number of CPUs if that is more, as pgxpool does.
	MaxConns int
	// MaxConnLifetime closes a connection given back after this long open;
	// MaxConnIdleTime one left unused this long. Zero means no limit.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
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

// maxConns is MaxConns, or its default when unset.
func (c PoolConfig) maxConns() int {
	if c.MaxConns > 0 {
		return c.MaxConns
	}
	return max(4, runtime.NumCPU())
}
