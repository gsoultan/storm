// Package benchmodel is the M0 benchmark table, declared as a model so the
// generator can be measured against the hand-written spike on identical SQL.
package benchmodel

import (
	"time"

	"github.com/gsoultan/storm"
)

type User struct {
	ID        storm.UUID
	OrgID     storm.UUID
	Email     string
	Name      string
	Age       *int32
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Projections gives the benchmark a narrow read to measure against the full
// one on identical predicates.
// Aggregates: the grouped read the benchmark measures. Its warm path has the
// same budget as every other read — zero allocations to build and bind.
func (u *User) Aggregates(a *storm.Aggregates) {
	a.Named("ByStatus").
		By(&u.Status).
		Count("Users").
		Avg(&u.Age, "AvgAge").
		Max(&u.CreatedAt, "Newest")
}

func (u *User) Projections(p *storm.Projections) {
	p.Named("Contact", &u.Email, &u.Name)
}

func All() []any { return []any{&User{}} }
