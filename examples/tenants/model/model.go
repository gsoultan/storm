// Package model is the sharded example's schema: one sharded table, one
// reference table copied to every shard, and nothing else.
//
// The whole sharding declaration is a single line in Order.Schema. Everything
// that follows from it — the shard.Bound parameter on every generated call,
// ShardKeyOf, the checking unit of work — is emitted, not written.
package model

import "github.com/gsoultan/storm"

// Order belongs to a tenant, and the tenant decides which DATABASE it is in.
type Order struct {
	storm.Model // uuid id + created_at/updated_at

	TenantID storm.UUID
	Currency *Currency
	Status   string
	Total    int32
}

func (o *Order) Schema(t *storm.Table) {
	// The one line. From here the compiler will not accept an Order query
	// that does not say which tenant it is for.
	t.ShardKey(&o.TenantID)

	t.Col(&o.Status).Size(16)
	t.Index(&o.TenantID, &o.Status)
}

// Currency is a REFERENCE table: small, rarely written, and copied to every
// shard. A sharded table may point at one — the child read runs on the
// parent's own shard, so it reads the copy that is there.
//
// Keeping that copy on every shard is the adopter's half of the arrangement.
// storm cannot check it from a model file; `storm verify` against each shard
// can.
type Currency struct {
	storm.Model

	Code string
}

func (c *Currency) Schema(t *storm.Table) {
	t.Col(&c.Code).Unique().Size(3)
}

// All is what the generator and the tests both read, so neither can drift
// from the other.
func All() []any { return []any{&Order{}, &Currency{}} }
