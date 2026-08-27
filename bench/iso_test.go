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
