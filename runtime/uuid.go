package runtime

// Client-side uuid generation, for a target whose server has none.
//
// PostgreSQL defaults a storm.Model's id with gen_random_uuid() or uuidv7() and
// the row arrives with a key it never had to be given. MySQL and MariaDB have
// no equivalent worth taking: UUID() is version 1, which embeds the SERVER'S
// MAC ADDRESS in a value that ends up in URLs, and it is a different version
// from the one the model asked for. So the key is generated here instead —
// version 7, so it stays time-ordered and a BINARY(16) primary key keeps its
// index locality.

import (
	"crypto/rand"
	"time"
)

// NewUUIDv7 returns a version 7 uuid: 48 bits of Unix milliseconds, then 74
// bits of randomness, with the version and variant bits set.
//
// Time-ordered on purpose. A v4 key scatters inserts across a btree and turns a
// primary key into a write amplifier; v7 sorts by creation, which is what makes
// it usable as a clustered key — and on MySQL the primary key IS the clustered
// index, so this matters more here than it does on PostgreSQL.
//
// crypto/rand, not math/rand: an id that appears in a URL is guessable if its
// randomness is.
func NewUUIDv7() [16]byte {
	var u [16]byte
	ms := uint64(time.Now().UnixMilli())
	// 48 bits of milliseconds, big-endian, so byte order IS time order.
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	if _, err := rand.Read(u[6:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, a predictable id is worse than no id at all.
		panic("storm: crypto/rand: " + err.Error())
	}
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u
}

// IsZeroUUID reports whether u is the zero value, which is how generated code
// tells "the caller did not set an id" from "the caller set one".
//
// The all-zero uuid is a legal value nobody means. Treating it as unset costs
// a caller who genuinely wanted it the ability to say so, which is a trade
// worth naming: a silently all-zero primary key makes the SECOND insert a
// duplicate-key error and the first row unfindable by anything but a scan.
func IsZeroUUID(u [16]byte) bool {
	var zero [16]byte
	return u == zero
}
