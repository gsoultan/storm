// Package schema is the Ent declaration for the shared bench table.
//
// It mirrors bench/schema.sql exactly — same table, same columns — because the
// comparison is of ORMs, not of two different schemas. Ent's generated client
// is committed like sqlc's is: the codegen step is part of what is being
// compared, and "needs its own codegen step" is the reason this rival was
// missing from M0 until now.
package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

type User struct {
	ent.Schema
}

func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}),
		field.UUID("org_id", uuid.UUID{}),
		field.String("email"),
		field.String("name"),
		field.Int32("age").Optional().Nillable(),
		field.String("status"),
		field.Time("created_at"),
		field.Time("updated_at"),
	}
}
