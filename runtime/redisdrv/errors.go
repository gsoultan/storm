// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package redisdrv

import "errors"

// Error is an error reply from the server: "-ERR …", "-WRONGTYPE …". The
// connection is still good after one.
type Error struct {
	Msg string
}

func (e *Error) Error() string { return "redis: " + e.Msg }

// ErrClosed is returned by a Client after Close.
var ErrClosed = errors.New("redisdrv: client is closed")

// errProtocol is a reply that does not parse. The connection it came from is
// discarded: nothing after it can be trusted to be aligned.
var errProtocol = errors.New("redisdrv: malformed reply")
