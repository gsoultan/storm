// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgsql_test

import (
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
)

// The document read as its text. COALESCE wraps the identifier so a missing
// document is the empty string, and a pattern list is one bound text[].
func TestJSONTextOperatorsReadTheDocumentAsText(t *testing.T) {
	for op, want := range map[string]string{
		"JSONTextLikeAny": `COALESCE("doc"::text, '') LIKE ANY($)`,
		"JSONIsEmpty":     `COALESCE("doc"::text, '') IN ('', 'null', '[]', '{}')`,
		"NullOrEmpty":     `COALESCE("doc", '') = ''`,
	} {
		a, b, ok := pgsql.Frag(op, pgsql.Ident("doc"))
		if !ok {
			t.Fatalf("no lowering for %s", op)
		}
		if got := a + b; got != want {
			t.Errorf("%s = %s, want %s", op, got, want)
		}
	}
}
