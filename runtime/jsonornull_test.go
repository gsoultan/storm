// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package runtime

import "testing"

func TestJSONOrNull(t *testing.T) {
	if got := JSON(nil).OrNull(); string(got) != "null" {
		t.Fatalf("nil: %q, want null", got)
	}
	if got := JSON(`{"a":1}`).OrNull(); string(got) != `{"a":1}` {
		t.Fatalf("a document: %q, want it unchanged", got)
	}
	var m map[string]any
	if err := JSON(nil).OrNull().Unmarshal(&m); err != nil || m != nil {
		t.Fatalf("null unmarshals to %v (%v), want the nil map nil did", m, err)
	}
}
