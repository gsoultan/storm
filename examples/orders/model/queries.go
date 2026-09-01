package model

import "github.com/gsoultan/storm"

// DailyRevenueRow is one row of the finance report.
type DailyRevenueRow struct {
	Day     string
	Orders  int64
	Revenue storm.Decimal
}

// DailyRevenue WAS the reason this file existed: the builder could not group by
// date_trunc('day', …), so the report had to be raw SQL. It can now, and the
// service uses the declared Order.Daily aggregation instead.
//
// Kept here because storm.SQL is still the answer for what the builder does not
// reach — a join projecting across tables, a CTE — and because it is worth
// seeing that both live in the same module and are both validated at generate
// time.
var DailyRevenue = storm.SQL[DailyRevenueRow](`
    SELECT to_char(placed_at, 'YYYY-MM-DD') AS day,
           count(*)                          AS orders,
           coalesce(sum(total), 0)::numeric(12,2) AS revenue
      FROM orders
     WHERE status <> 'cancelled'
       AND placed_at >= $1
     GROUP BY 1
     ORDER BY 1 DESC`)

// ReleaseExpiredHolds is a statement, not a query: no rows, just a count.
var ReleaseExpiredHolds = storm.SQLExec(`
    UPDATE stock_items s
       SET reserved = 0, version = version + 1
      FROM orders o
     WHERE o.status = 'pending'
       AND o.placed_at < $1`)
