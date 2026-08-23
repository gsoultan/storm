package spike

import (
	"encoding/binary"
	"time"
)

// Null is the allocation-free nullable, mirroring what the generator would emit
// for a `*T` field in the model.
type Null[T any] struct {
	V     T
	Valid bool
}

func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// Row is what a full read returns: the model struct with relations replaced by
// their scalar foreign keys and *T rewritten to Null[T].
type Row struct {
	ID        [16]byte
	OrgID     [16]byte
	Email     string
	Name      string
	Age       Null[int32]
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// pgEpoch is Postgres' timestamp origin: 2000-01-01 00:00:00 UTC.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// scanRow decodes one row straight from the wire into r. This is the code the
// generator emits: no reflect, no `any`, no driver.Value boxing. Text columns
// are copied into the caller's Slab, so a result costs a handful of
// allocations rather than three per row.
func scanRow(rv [][]byte, r *Row, sl *Slab) {
	copy(r.ID[:], rv[0])
	copy(r.OrgID[:], rv[1])
	r.Email = sl.str(rv[2])
	r.Name = sl.str(rv[3])
	if rv[4] == nil {
		r.Age = Null[int32]{}
	} else {
		r.Age = Null[int32]{V: int32(binary.BigEndian.Uint32(rv[4])), Valid: true}
	}
	r.Status = sl.str(rv[5])
	r.CreatedAt = pgTimestamptz(rv[6])
	r.UpdatedAt = pgTimestamptz(rv[7])
}

func pgTimestamptz(b []byte) time.Time {
	if b == nil {
		return time.Time{}
	}
	return pgEpoch.Add(time.Duration(int64(binary.BigEndian.Uint64(b))) * time.Microsecond)
}

// DecodeRowForTest exposes the generated-style scanner so its cost can be
// measured without a network round trip in the way.
func DecodeRowForTest(rv [][]byte, r *Row, s *Slab) { scanRow(rv, r, s) }
