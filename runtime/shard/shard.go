// Package shard routes a query to the one database that holds its rows.
//
// storm shards by ROUTING, not by fanning out. A sharded model declares its
// shard key in its Schema, and every generated call on it takes a Bound —
// an Executor that has already been resolved to one shard — rather than a
// plain Executor. Handing such a call a pool is a COMPILE error, not a runtime
// one, which is the same bargain the rest of storm makes: the thing that
// cannot be checked later is checked by the type system now.
//
//	shards, _ := shard.New(shard.Jump(4), db0, db1, db2, db3)
//	ex, _ := shards.For(shard.UUIDKey(tenantID))
//	us, err := user.New().StatusEq("active").All(ctx, ex, nil)
//
// What this package does NOT do is as load-bearing as what it does:
//
//   - No fan-out. There is no "run it on every shard and merge", because a
//     correct merge has to re-apply ORDER BY, LIMIT and keyset pagination
//     across streams, and has no correct answer at all for AVG or for a
//     window function. Each, below, is the honest version: it hands you each
//     shard in turn and lets you decide what combining means.
//
//   - No cross-shard transaction. A transaction is one shard. A Unit that
//     stages writes for two of them is refused by name — see Unit — rather
//     than committed halfway. Two-phase commit would need a durable
//     coordinator and a recovery process, which is a deployed thing, and
//     storm is imported rather than deployed.
//
//   - No resharding. Moving a tenant between shards is a data migration with
//     a cutover, and a library that offered to do it live would be lying
//     about the hard part.
//
//   - No Set.Migrate. migrate.AutoPool takes a *pgxpool.Pool and a Set holds
//     runtime.DB, so migration happens BEFORE a Set exists, while the caller
//     still holds the pools they are about to hand over:
//
//     for _, p := range pools {
//     if _, err := migrate.AutoPool(ctx, p, model, opts); err != nil { ... }
//     }
//     shards, err := shard.New(shard.Jump(len(pools)), dbs...)
//
//     Auto takes an advisory lock per DATABASE, so several instances racing
//     to migrate one shard is safe. Nothing makes four shards atomic with
//     each other: a rollout that dies after shard 1 leaves shards 2 and 3 on
//     the old schema, serving reads that succeed. That is an operator's
//     problem and storm says so rather than pretending to solve it.
package shard

import (
	"errors"
	"strconv"
)

// ID is a shard's position in the Set: 0 to N-1.
//
// A position rather than a name, because the locators that compute one —
// Jump and Modulo — compute a position and would otherwise need a second
// table to translate it. A Table locator maps whatever names an adopter uses
// onto these.
type ID int

// ErrNoShard means a key resolved to a shard the Set does not have. From
// Table, it means the key is not in the map; from Jump or Modulo it means the
// locator was built for a different shard count than the Set holds.
var ErrNoShard = errors.New("storm/shard: no shard for this key")

// Key is the value of a model's shard key, in one of the three shapes a shard
// key comes in: a string, an int64, or a UUID.
//
// A struct rather than an `any` because this value is built on the path of
// every query against a sharded table, and boxing an int64 into an interface
// allocates. A struct rather than three Locator methods because Key is
// COMPARABLE, which is what lets Table be a plain map[Key]ID and handle all
// three shapes with one lookup.
//
// The three shapes are not a guess: they are what storm's key columns are.
// storm.Model's id is a UUID, a tenant slug is a string, and a legacy
// integer key is an int64. A shard key of any other type is refused at
// generate time, where the model can be named.
type Key struct {
	s    string
	n    int64
	u    [16]byte
	kind kind
}

type kind uint8

const (
	kindInvalid kind = iota
	kindString
	kindInt64
	kindUUID
)

// StringKey builds a shard key from a text column.
func StringKey(s string) Key { return Key{s: s, kind: kindString} }

// Int64Key builds a shard key from an integer column.
func Int64Key(n int64) Key { return Key{n: n, kind: kindInt64} }

// UUIDKey builds a shard key from a uuid column — the common case, because
// storm.Model's id is a client-generated UUID.
func UUIDKey(u [16]byte) Key { return Key{u: u, kind: kindUUID} }

// String renders the key for an error message. It is not the hash and is not
// used for routing; it exists so that a refusal can say WHICH tenant crossed
// a shard boundary instead of just that one did.
//
// Written out by hand rather than formatted: scripts/check forbids string
// formatting anywhere under runtime/. The rule is about SQL being assembled on
// a hot path and this is neither, but a blanket rule with one exception is a
// rule nobody can grep for. strconv and a hex table cost less anyway.
func (k Key) String() string {
	switch k.kind {
	case kindString:
		return k.s
	case kindInt64:
		return strconv.FormatInt(k.n, 10)
	case kindUUID:
		const hex = "0123456789abcdef"
		out := make([]byte, 0, 36)
		for i, b := range k.u {
			if i == 4 || i == 6 || i == 8 || i == 10 {
				out = append(out, '-')
			}
			out = append(out, hex[b>>4], hex[b&0x0f])
		}
		return string(out)
	default:
		return "<invalid>"
	}
}

// hash is FNV-1a over the key's bytes, with the shape mixed in first so that
// StringKey("1") and Int64Key(1) do not collide.
//
// Written out rather than calling hash/fnv because that returns an interface
// whose Write takes a []byte: hashing a string through it would convert, and
// converting allocates. This version ranges the string by index and never
// leaves the stack — asserted by a test, because "no allocation on the query
// path" is a claim and not a hope.
func (k Key) hash() uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	h = (h ^ uint64(k.kind)) * prime64
	switch k.kind {
	case kindString:
		for i := 0; i < len(k.s); i++ {
			h = (h ^ uint64(k.s[i])) * prime64
		}
	case kindInt64:
		u := uint64(k.n)
		for i := 0; i < 8; i++ {
			h = (h ^ (u & 0xff)) * prime64
			u >>= 8
		}
	case kindUUID:
		for i := 0; i < 16; i++ {
			h = (h ^ uint64(k.u[i])) * prime64
		}
	}
	return h
}
