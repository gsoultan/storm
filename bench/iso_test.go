// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package bench

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/bench/genuser"
	"github.com/gsoultan/storm/internal/spike"
)

func BenchmarkIso_GenShapeOnly(b *testing.B) {
	q := genDynamic6()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if q.Shape() == 0 {
			b.Fatal()
		}
	}
}

func BenchmarkIso_GenCacheOnly(b *testing.B) {
	q := genDynamic6()
	q.SQL()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if q.SQL() == "" {
			b.Fatal()
		}
	}
}

func BenchmarkIso_GenBindOnly(b *testing.B) {
	q := genDynamic6()
	q.SQL()
	bd := genuser.GetBinder()
	defer genuser.PutBinder(bd)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, args := q.Prepare(bd)
		if len(args) != 7 {
			b.Fatal()
		}
	}
}

func BenchmarkIso_SpikeCacheOnly(b *testing.B) {
	q := dynamic6()
	q.SQL()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if q.SQL() == "" {
			b.Fatal()
		}
	}
}

func BenchmarkIso_SpikeBindOnly(b *testing.B) {
	q := dynamic6()
	q.SQL()
	bd := spike.GetBinder()
	defer spike.PutBinder(bd)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, args := q.Prepare(bd)
		if len(args) != 7 {
			b.Fatal()
		}
	}
}

var _ = time.Now
