// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"time"

	"github.com/gsoultan/caduceus"
)

// member is one connection of a Pool: when it was opened, for
// MaxConnLifetime, and when it was last given back, for MaxConnIdleTime.
type member struct {
	conn     *caduceus.Conn
	born     time.Time
	returned time.Time
}

// stale reports whether the member has outlived the pool's limits at now.
func (m *member) stale(c PoolConfig, now time.Time) bool {
	return (c.MaxConnLifetime > 0 && now.Sub(m.born) >= c.MaxConnLifetime) ||
		(c.MaxConnIdleTime > 0 && now.Sub(m.returned) >= c.MaxConnIdleTime)
}
