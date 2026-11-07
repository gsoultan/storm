// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mydrv

import "time"

// ProbeIdleAfter sets how long a pooled connection sits idle before it is
// probed, and returns the function that restores the default. Exported for the
// live tests, which cannot wait out a second per case.
func ProbeIdleAfter(d time.Duration) (restore func()) {
	old := idleProbeAfter
	idleProbeAfter = d
	return func() { idleProbeAfter = old }
}
