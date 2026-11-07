package redisdrv

import (
	"bufio"
	"strings"
	"testing"
)

// A server-claimed array length is not an allocation size. Before, MaxReplyBytes
// bounded each bulk string and each array header separately, so a 14-byte
// `*67108864` made the client allocate a 1 GiB []any before reading a single
// element, and an array of bulk strings could cost MaxReplyBytes per element.
// The bound is now one budget for the whole reply.
func TestArrayHeaderCannotAmplifyAllocation(t *testing.T) {
	const max = 64 << 20
	allocs := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			r := bufio.NewReader(strings.NewReader("*67108864\r\n:1\r\n"))
			if _, err := readReply(r, max); err == nil {
				b.Fatal("a truncated 64 Mi-element array read clean")
			}
		}
	})
	if per := allocs.AllocedBytesPerOp(); per > 64<<10 {
		t.Fatalf("a 14-byte header cost %d bytes", per)
	}

	// Two bulk strings each just under the budget overrun it together.
	half := strings.Repeat("x", 600)
	raw := "*2\r\n$600\r\n" + half + "\r\n$600\r\n" + half + "\r\n"
	if _, err := readReply(bufio.NewReader(strings.NewReader(raw)), 1000); err == nil {
		t.Fatal("a reply over the whole-reply budget read clean")
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader(raw)), 2000); err != nil {
		t.Fatalf("a reply inside the budget: %v", err)
	}
}

// FuzzReadReply: no server bytes panic the reader, and a reply it accepts never
// holds more payload than the budget.
func FuzzReadReply(f *testing.F) {
	for _, s := range []string{
		"+OK\r\n", "-ERR x\r\n", ":42\r\n", "$3\r\nabc\r\n", "$-1\r\n", "*-1\r\n",
		"*2\r\n$1\r\na\r\n:1\r\n", "*1\r\n*1\r\n*1\r\n:1\r\n", "*3\r\n", "$99999\r\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		const budget = 4096
		v, err := readReply(bufio.NewReaderSize(strings.NewReader(string(in)), 4096), budget)
		if err != nil {
			return
		}
		if n := payload(v); n > budget {
			t.Fatalf("accepted %d payload bytes over a %d budget", n, budget)
		}
	})
}

func payload(v any) int {
	switch x := v.(type) {
	case []byte:
		return len(x)
	case string:
		return len(x)
	case *Error:
		return len(x.Msg)
	case []any:
		n := 0
		for _, e := range x {
			n += payload(e)
		}
		return n
	}
	return 0
}
