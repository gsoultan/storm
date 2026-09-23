package storm

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// Shard-key validation — what a sharding declaration can get wrong that no
// database will ever report.
//
// This pass is different in kind from the index and aggregate ones beside it.
// Those refuse things the server would reject, earlier and with a better
// message; the server is still a backstop. Here there is no backstop at all.
// Each shard holds an ordinary table with ordinary constraints, and nothing
// in any of them records that three other servers hold the rest of the rows.
// A query sent to the wrong shard does not fail — it returns fewer rows, and
// every constraint it touched was satisfied. So the only place a sharding
// mistake can be caught is here, before the code that would make it exists.

// shardKeyTypes are the column types a shard key may have: the three shapes
// shard.Key holds. A timestamp is absent because sharding by time puts every
// write on one shard, and a bool because two shards is not sharding.
var shardKeyTypes = map[string]bool{
	schema.TypeUUID:    true,
	schema.TypeText:    true,
	schema.TypeInt2:    true,
	schema.TypeInt4:    true,
	schema.TypeInt8:    true,
	schema.TypeVarchar: true,
}

func (b *builder) validateShardKeys() {
	sharded := map[string]string{} // table name -> shard key column
	for _, mi := range b.ordered {
		if t := mi.tbl.out; t.Sharded() {
			sharded[t.Name] = t.ShardKey
		}
	}
	if len(sharded) == 0 {
		return
	}
	for _, mi := range b.ordered {
		b.validateShardKey(mi.tbl.out, sharded)
	}
}

func (b *builder) validateShardKey(t *schema.Table, sharded map[string]string) {
	fail := func(format string, a ...any) {
		b.errs.add(fmt.Errorf("%s: "+format, append([]any{t.Name}, a...)...))
	}

	// The column, checked against the FINAL schema rather than against what it
	// looked like when ShardKey was called. `t.ShardKey(&o.TenantID)` followed
	// by `t.Col(&o.TenantID).Null()` in the same Schema method is a model that
	// passed the check at the call and is wrong by the end of it.
	if t.Sharded() {
		c := t.Column(t.ShardKey)
		switch {
		case c == nil:
			fail("the shard key names column %q, which this table does not have", t.ShardKey)
		case !c.NotNull:
			fail("%s is the shard key and is nullable — NULL names no shard, so a row with one "+
				"could not be written anywhere or found again", c.Name)
		case !shardKeyTypes[c.Type.Name]:
			fail("%s is the shard key and is %s — a shard key is a uuid, text or an integer, "+
				"because those are the shapes shard.Key holds", c.Name, c.Type.Name)
		}
	}

	for _, r := range t.Relations {
		targetKey, targetSharded := sharded[r.Target]

		switch {
		case !t.Sharded() && targetSharded:
			// The unsharded side's generated calls take a runtime.Executor,
			// which names no shard — so there is nothing to route the child
			// read with, and it would go to whichever database the caller
			// happened to hold. Unlike the reverse direction below, there is
			// no deployment in which this is correct.
			fail("%s points at %s, which is sharded by %s, and %s is not sharded — a read from "+
				"here carries no shard, so the %s rows would be fetched from whichever database "+
				"the caller passed. Shard %s by the same key, or drop the relation and look the "+
				"rows up through the shard set",
				r.Field, r.Target, targetKey, t.Name, r.Target, t.Name)

		case t.Sharded() && targetSharded && targetKey != t.ShardKey:
			// Two shard keys on one join is two answers for where the joined
			// row lives, and the query can only send one of them.
			fail("%s is sharded by %s and points at %s, which is sharded by %s — the two rows "+
				"can land on different databases and the read would find only the ones that did "+
				"not. Shard both by the same column, or denormalise %s onto %s",
				t.Name, t.ShardKey, r.Target, targetKey, targetKey, t.Name)
		}

		// t sharded, target NOT sharded is allowed, and is the reference-table
		// case: `countries`, `plans`, `currencies` — small, rarely written,
		// and copied to every shard. The child read runs against the parent's
		// own Bound, so it reads the copy on that shard, which is right IF the
		// copy is there. storm cannot check that a table exists on every shard
		// from a model file, so this is the one part of the arrangement the
		// adopter owns; `storm verify` run against each shard is what proves
		// it.
	}
}
