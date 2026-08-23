package bench

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gsoultan/raorm/internal/spike"
)

// TestSpikeMatchesPgx runs every one of the 64 shapes through both the spike
// and hand-written pgx, and requires identical rows. A fast wrong answer is
// worth nothing.
func TestSpikeMatchesPgx(t *testing.T) {
	ctx := context.Background()
	ex := spike.PgxExec{Pool: pool}
	since := time.Now().Add(-400 * 24 * time.Hour)

	for mask := uint32(0); mask < spike.NumShapes; mask++ {
		q := spike.New().Limit(25)
		if mask&1 != 0 {
			q = q.Org(orgs[3])
		}
		if mask&2 != 0 {
			q = q.Email("user000003@corp.com")
		}
		if mask&4 != 0 {
			q = q.NameLike("User 0000%")
		}
		if mask&8 != 0 {
			q = q.AgeGte(21)
		}
		if mask&16 != 0 {
			q = q.Status("active")
		}
		if mask&32 != 0 {
			q = q.Since(since)
		}
		if q.Shape() != mask {
			t.Fatalf("shape %d: builder produced %d", mask, q.Shape())
		}

		got, err := q.All(ctx, ex, nil)
		if err != nil {
			t.Fatalf("shape %d spike: %v", mask, err)
		}

		// Same SQL, executed straight through pgx with an idiomatic scan.
		rows, err := pool.Query(ctx, q.SQL(), argsFor(mask, since)...)
		if err != nil {
			t.Fatalf("shape %d pgx: %v", mask, err)
		}
		var want []spike.Row
		for rows.Next() {
			want = append(want, spike.Row{})
			if err := scanPgx(rows, &want[len(want)-1]); err != nil {
				t.Fatal(err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}

		if len(got) != len(want) {
			t.Fatalf("shape %d (%s): got %d rows, want %d", mask, q.SQL(), len(got), len(want))
		}
		for i := range got {
			if !got[i].CreatedAt.Equal(want[i].CreatedAt) || !got[i].UpdatedAt.Equal(want[i].UpdatedAt) {
				t.Fatalf("shape %d row %d: timestamp mismatch\n got %+v\nwant %+v", mask, i, got[i], want[i])
			}
			g, w := got[i], want[i]
			g.CreatedAt, g.UpdatedAt = time.Time{}, time.Time{}
			w.CreatedAt, w.UpdatedAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("shape %d row %d mismatch\n got %+v\nwant %+v", mask, i, g, w)
			}
		}
	}
}

func argsFor(mask uint32, since time.Time) []any {
	var a []any
	if mask&1 != 0 {
		a = append(a, orgs[3])
	}
	if mask&2 != 0 {
		a = append(a, "user000003@corp.com")
	}
	if mask&4 != 0 {
		a = append(a, "User 0000%")
	}
	if mask&8 != 0 {
		a = append(a, int32(21))
	}
	if mask&16 != 0 {
		a = append(a, "active")
	}
	if mask&32 != 0 {
		a = append(a, since)
	}
	return append(a, int32(25))
}

// TestFastPathMatchesPgx re-runs all 64 shapes through the pgconn fast path.
// A faster wrong answer is worth less than a slow right one.
func TestFastPathMatchesPgx(t *testing.T) {
	ctx := context.Background()
	slow := spike.PgxExec{Pool: pool}
	fast := spike.FastExec{Pool: pool}
	since := time.Now().Add(-400 * 24 * time.Hour)

	for mask := uint32(0); mask < spike.NumShapes; mask++ {
		q := shapeQuery(mask, since)
		want, err := q.All(ctx, slow, nil)
		if err != nil {
			t.Fatalf("shape %d slow: %v", mask, err)
		}
		var sl spike.Slab
		got, err := q.AllFast(ctx, fast, nil, &sl)
		if err != nil {
			t.Fatalf("shape %d fast: %v", mask, err)
		}
		if len(got) != len(want) {
			t.Fatalf("shape %d: fast %d rows, slow %d", mask, len(got), len(want))
		}
		for i := range got {
			if !got[i].CreatedAt.Equal(want[i].CreatedAt) {
				t.Fatalf("shape %d row %d: created_at %v vs %v", mask, i, got[i].CreatedAt, want[i].CreatedAt)
			}
			g, w := got[i], want[i]
			g.CreatedAt, g.UpdatedAt, w.CreatedAt, w.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("shape %d row %d mismatch\n fast %+v\n slow %+v", mask, i, g, w)
			}
		}
	}
}

func shapeQuery(mask uint32, since time.Time) spike.Query {
	q := spike.New().Limit(25)
	if mask&1 != 0 {
		q = q.Org(orgs[3])
	}
	if mask&2 != 0 {
		q = q.Email("user000003@corp.com")
	}
	if mask&4 != 0 {
		q = q.NameLike("User 0000%")
	}
	if mask&8 != 0 {
		q = q.AgeGte(21)
	}
	if mask&16 != 0 {
		q = q.Status("active")
	}
	if mask&32 != 0 {
		q = q.Since(since)
	}
	return q
}
