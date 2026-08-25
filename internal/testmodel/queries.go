package testmodel

import (
	"github.com/gsoultan/raorm"
)

// EarnerRow is the M5 gate query's row: user-declared, matched against the
// prepared statement's descriptor at generate time.
type EarnerRow struct {
	Email    string
	OrgName  string
	Rank     int64
	OrgUsers int64
}

// TopPerOrg is the gate query itself: a window over a CTE with a lateral join.
var TopPerOrg = raorm.SQL[EarnerRow](`
	WITH ranked AS (
		SELECT u.email, u.org_id,
		       row_number() OVER (PARTITION BY u.org_id ORDER BY u.email) AS rn
		FROM users u
	)
	SELECT r.email, o.name AS org_name, r.rn AS rank, l.org_users
	FROM ranked r
	JOIN orgs o ON o.id = r.org_id
	JOIN LATERAL (
		SELECT count(*) AS org_users FROM users u2 WHERE u2.org_id = r.org_id
	) l ON true
	WHERE r.rn <= $1
	ORDER BY o.name, r.rn
	LIMIT $2`)

// Queries is what a bootstrap registers, the way All registers models.
func Queries() []raorm.RawDecl {
	return []raorm.RawDecl{TopPerOrg}
}
