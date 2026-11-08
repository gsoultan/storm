// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package tool

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/examples/blog/model"
)

// The quickstart's model does not port, and the refusal says why in terms an
// adopter can act on — beginning with the one ADR-0004 makes mandatory.
func TestPortableMongoRefusesTheBlogModelByName(t *testing.T) {
	s, err := storm.Build(model.All()...)
	if err != nil {
		t.Fatal(err)
	}
	err = portable("mongodb", s)
	if err == nil {
		t.Fatal("the blog model ports to MongoDB, apparently")
	}
	if !strings.Contains(err.Error(), "OnDocument(storm.Reference())") {
		t.Errorf("the refusal must name the missing OnDocument:\n%v", err)
	}
	// The Feed union is a $unionWith now, so it is no longer refused.
	if strings.Contains(err.Error(), "union Feed") {
		t.Errorf("the Feed union lowers to $unionWith, yet:\n%v", err)
	}
}
