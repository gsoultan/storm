package msdrv

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

// bytesConn is a net.Conn that reads from a fixed buffer.
type bytesConn struct {
	net.Conn
	r *bytes.Reader
}

func (c bytesConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// tdsMessage frames payload as one EOM packet, as the server would.
func tdsMessage(payload []byte) []byte {
	h := make([]byte, headerSize, headerSize+len(payload))
	h[0] = 4
	h[1] = statusEOM
	binary.BigEndian.PutUint16(h[2:], uint16(headerSize+len(payload)))
	return append(h, payload...)
}

func plpConn(payload []byte) *conn {
	return &conn{c: bytesConn{r: bytes.NewReader(tdsMessage(payload))}, rbuf: make([]byte, 4096)}
}

// A PLP value's declared total and its chunk lengths are the server's claims.
// readPLP sized its buffer from the total, so eight bytes claiming 2^62 made
// it try to allocate that much (and a total past MaxInt panicked makeslice),
// and a chunk header claiming 4 GiB allocated 4 GiB before reading a byte.
func TestPLPClaimsDoNotSizeAllocations(t *testing.T) {
	for _, c := range []struct {
		name  string
		total uint64
		chunk uint32
	}{
		{"total past MaxInt", 1<<64 - 3, 3},
		{"huge total", 1 << 62, 3},
		{"huge chunk", plpUnknown, 1<<32 - 1},
	} {
		var p []byte
		p = binary.LittleEndian.AppendUint64(p, c.total)
		p = binary.LittleEndian.AppendUint32(p, c.chunk)
		p = append(p, "abc"...) // all the server ever sends
		var scratch []byte
		allocs := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				scratch = nil
				_, _ = plpConn(p).readPLP(&scratch)
			}
		})
		if per := allocs.AllocedBytesPerOp(); per > 8<<20 {
			t.Errorf("%s: %d bytes allocated for a 3-byte value", c.name, per)
		}
	}

	// A well-formed value still reads, across chunks.
	var p []byte
	p = binary.LittleEndian.AppendUint64(p, 5)
	p = binary.LittleEndian.AppendUint32(p, 2)
	p = append(p, "he"...)
	p = binary.LittleEndian.AppendUint32(p, 3)
	p = append(p, "llo"...)
	p = binary.LittleEndian.AppendUint32(p, 0)
	var scratch []byte
	got, err := plpConn(p).readPLP(&scratch)
	if err != nil || string(got) != "hello" {
		t.Fatalf("readPLP = %q, %v", got, err)
	}
}

// FuzzReadPLP: no PLP stream panics the reader.
func FuzzReadPLP(f *testing.F) {
	f.Add([]byte{5, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 'h', 'e', 'l', 'l', 'o', 0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 4000 {
			return
		}
		var scratch []byte
		_, _ = plpConn(payload).readPLP(&scratch)
	})
}
