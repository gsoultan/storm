package model

import "github.com/gsoultan/storm/v2"

type Order struct {
	storm.Model
	TenantID storm.UUID
	Status   string
}

func (o *Order) Schema(t *storm.Table) { t.ShardKey(&o.TenantID) }
