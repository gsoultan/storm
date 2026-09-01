package shared

import "github.com/gsoultan/storm"

// Base is a cross-package mixin: exported, has a Schema method, and is not a
// table. Only the fact that something embeds it says so.
type Base struct {
	Version int32
}

func (b *Base) Schema(t *storm.Table) { t.Col(&b.Version).Version() }
