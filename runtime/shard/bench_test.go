package shard

// What routing costs.
//
// A shard lookup happens on the path of every query against a sharded table,
// so its cost is paid per query and is worth a number rather than an
// adjective. The allocation count is asserted in shard_test.go; this is the
// time.

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func BenchmarkJumpLocate(b *testing.B) {
	loc := Jump(8)
	k := UUIDKey([16]byte{0x01, 0x02, 0x03, 0x04})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := loc.Locate(k); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkModuloLocate(b *testing.B) {
	loc := Modulo(8)
	k := UUIDKey([16]byte{0x01, 0x02, 0x03, 0x04})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := loc.Locate(k); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTableLocate(b *testing.B) {
	keys := make(map[Key]ID, 1000)
	for i := range 1000 {
		keys[Int64Key(int64(i))] = ID(i % 8)
	}
	loc, err := NewTable(8, keys)
	if err != nil {
		b.Fatal(err)
	}
	k := Int64Key(500)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := loc.Locate(k); err != nil {
			b.Fatal(err)
		}
	}
}

// The whole routing step an application pays: locate, bounds-check, and build
// the Bound the generated call will take.
func BenchmarkSetFor(b *testing.B) {
	dbs := make([]runtime.DB, 8)
	for i := range dbs {
		dbs[i] = &fakeDB{}
	}
	s, err := New(Jump(8), dbs...)
	if err != nil {
		b.Fatal(err)
	}
	k := UUIDKey([16]byte{0x01, 0x02, 0x03, 0x04})
	b.ReportAllocs()
	for b.Loop() {
		ex, err := s.For(k)
		if err != nil {
			b.Fatal(err)
		}
		_ = ex.Shard()
	}
}

// A string key is the one shape whose hash walks a variable number of bytes,
// so it is the one whose cost depends on the data.
func BenchmarkSetForStringKey(b *testing.B) {
	dbs := make([]runtime.DB, 8)
	for i := range dbs {
		dbs[i] = &fakeDB{}
	}
	s, err := New(Jump(8), dbs...)
	if err != nil {
		b.Fatal(err)
	}
	k := StringKey("a-fairly-long-tenant-slug-as-they-go")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.For(k); err != nil {
			b.Fatal(err)
		}
	}
}
