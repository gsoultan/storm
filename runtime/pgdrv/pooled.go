// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

import (
	"time"

	"github.com/gsoultan/caduceus"
)

// pooled is a pool's connection and when it was last given back.
type pooled struct {
	c         *caduceus.Conn
	idleSince time.Time
}
