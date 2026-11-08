// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package schema

import "testing"

// ColumnName is what a model field maps to, and what a raw row type's fields
// are matched against. The acronym cases are the ones the two sides once
// disagreed on.
func TestColumnName(t *testing.T) {
	for field, want := range map[string]string{
		"ID":         "id",
		"OrgID":      "org_id",
		"CPUPercent": "cpu_percent",
		"HTTPServer": "http_server",
		"CpuPercent": "cpu_percent",
		"Total2":     "total2",
		"P95Latency": "p95_latency",
	} {
		if got := ColumnName(field); got != want {
			t.Errorf("ColumnName(%q) = %q, want %q", field, got, want)
		}
	}
}
