// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package redisdrv

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"time"
)

// conn is one connection, used by one caller at a time.
type conn struct {
	nc  net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
	max int
	bad bool // an I/O or protocol error: discard, never reuse
}

func dial(ctx context.Context, o Options) (*conn, error) {
	ctx, cancel := context.WithTimeout(ctx, o.DialTimeout)
	defer cancel()
	d := net.Dialer{}
	nc, err := d.DialContext(ctx, "tcp", o.Addr)
	if err != nil {
		return nil, err
	}
	if o.TLS != nil {
		cfg := o.TLS.Clone()
		if cfg.ServerName == "" {
			if host, _, err := net.SplitHostPort(o.Addr); err == nil {
				cfg.ServerName = host
			}
		}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	c := &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc), max: o.MaxReplyBytes}

	var hello [][]string
	switch {
	case o.Username != "":
		hello = append(hello, []string{"AUTH", o.Username, o.Password})
	case o.Password != "":
		hello = append(hello, []string{"AUTH", o.Password})
	}
	if o.DB != 0 {
		hello = append(hello, []string{"SELECT", strconv.Itoa(o.DB)})
	}
	if len(hello) > 0 {
		replies, err := c.do(ctx, o.DialTimeout, hello...)
		if err == nil {
			for _, r := range replies {
				if e, ok := r.(*Error); ok {
					err = e
					break
				}
			}
		}
		if err != nil {
			nc.Close()
			return nil, err
		}
	}
	return c, nil
}

// do sends cmds in one write and reads one reply per command: a pipeline, so
// N commands cost one round trip.
//
// The deadline is the earlier of ctx's and now+timeout, and a cancellation
// that arrives mid-read moves it to the past, which unblocks the read. Either
// way the connection is marked bad: a reply may be half-read.
func (c *conn) do(ctx context.Context, timeout time.Duration, cmds ...[]string) ([]any, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.nc.SetDeadline(deadline); err != nil {
		c.bad = true
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.nc.SetDeadline(time.Unix(1, 0)) })
	replies, err := c.roundTrip(cmds)
	if !stop() {
		c.bad = true
		if err == nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		c.bad = true
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	}
	return replies, nil
}

func (c *conn) roundTrip(cmds [][]string) ([]any, error) {
	for _, cmd := range cmds {
		if err := writeCommand(c.w, cmd); err != nil {
			return nil, err
		}
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	out := make([]any, len(cmds))
	for i := range out {
		var err error
		if out[i], err = readReply(c.r, c.max); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *conn) close() error { return c.nc.Close() }
