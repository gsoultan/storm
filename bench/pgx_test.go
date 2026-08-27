package bench

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/internal/spike"
	"github.com/jackc/pgx/v5/pgtype"
)

const cols = `SELECT id, org_id, email, name, age, status, created_at, updated_at FROM users`

// scanPgx is the hand-written scan an experienced pgx user writes: pgtype.Int4
// for the nullable column so nothing is boxed into a pointer.
func scanPgx(rows interface {
	Scan(...any) error
}, r *spike.Row) error {
	var age pgtype.Int4
	if err := rows.Scan(&r.ID, &r.OrgID, &r.Email, &r.Name, &age,
		&r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return err
	}
	r.Age = spike.Null[int32]{V: age.Int32, Valid: age.Valid}
	return nil
}

func BenchmarkGet_Pgx(b *testing.B) {
	ctx := context.Background()
	const q = cols + ` WHERE id = $1`
	var r spike.Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := pool.Query(ctx, q, ids[i%len(ids)])
		if err != nil {
			b.Fatal(err)
		}
		if !rows.Next() {
			rows.Close()
			b.Fatal("no row")
		}
		if err := scanPgx(rows, &r); err != nil {
			b.Fatal(err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScan1000_Pgx(b *testing.B) {
	ctx := context.Background()
	const q = cols + ` WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2`
	buf := make([]spike.Row, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := pool.Query(ctx, q, "active", 1000)
		if err != nil {
			b.Fatal(err)
		}
		buf = buf[:0]
		for rows.Next() {
			buf = append(buf, spike.Row{})
			if err := scanPgx(rows, &buf[len(buf)-1]); err != nil {
				b.Fatal(err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		if len(buf) != 1000 {
			b.Fatal(len(buf))
		}
	}
}

// BenchmarkDynamic6_Pgx is what a pgx user actually writes for a dynamic
// filter set: build the string, append the args. Rebuilt on every call,
// because there is nowhere to cache it.
func BenchmarkDynamic6_Pgx(b *testing.B) {
	ctx := context.Background()
	since := time.Now().Add(-400 * 24 * time.Hour)
	buf := make([]spike.Row, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sb strings.Builder
		sb.WriteString(cols)
		args := make([]any, 0, 7)
		add := func(frag string, v any) {
			if len(args) == 0 {
				sb.WriteString(" WHERE ")
			} else {
				sb.WriteString(" AND ")
			}
			args = append(args, v)
			sb.WriteString(frag)
			sb.WriteString(strconv.Itoa(len(args)))
		}
		add("org_id = $", orgs[3])
		add("email = $", "user000003@corp.com")
		add("name LIKE $", "User %")
		add("age >= $", int32(21))
		add("status = $", "active")
		add("created_at >= $", since)
		sb.WriteString(" ORDER BY created_at DESC, id LIMIT $")
		args = append(args, int32(50))
		sb.WriteString(strconv.Itoa(len(args)))

		rows, err := pool.Query(ctx, sb.String(), args...)
		if err != nil {
			b.Fatal(err)
		}
		buf = buf[:0]
		for rows.Next() {
			buf = append(buf, spike.Row{})
			if err := scanPgx(rows, &buf[len(buf)-1]); err != nil {
				b.Fatal(err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
	}
}
