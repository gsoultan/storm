// Package benchmodel is the M0 benchmark table, declared as a model so the
// generator can be measured against the hand-written spike on identical SQL.
package benchmodel

import (
	"time"

	"github.com/gsoultan/raorm"
)

type User struct {
	ID        raorm.UUID
	OrgID     raorm.UUID
	Email     string
	Name      string
	Age       *int32
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func All() []any { return []any{&User{}} }
