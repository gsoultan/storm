package runtime_test

import (
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// eachOf stands in for a generated Each: it calls fn per value and reports
// whether it was stopped, which is what closing the result depends on.
func eachOf(vals []int, err error, stopped *bool) func(func(int) error) error {
	return func(fn func(int) error) error {
		for _, v := range vals {
			if e := fn(v); e != nil {
				*stopped = true
				return e
			}
		}
		return err
	}
}

func TestSeqYieldsEveryRow(t *testing.T) {
	var stopped bool
	var got []int
	for v, err := range runtime.Seq(eachOf([]int{1, 2, 3}, nil, &stopped)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if len(got) != 3 || got[2] != 3 || stopped {
		t.Fatalf("got %v, stopped=%v", got, stopped)
	}
}

// break must reach Each as an error, or the result set stays open behind a
// loop that has already moved on.
func TestSeqBreakStopsEach(t *testing.T) {
	var stopped bool
	n := 0
	for range runtime.Seq(eachOf([]int{1, 2, 3, 4}, nil, &stopped)) {
		n++
		if n == 2 {
			break
		}
	}
	if !stopped || n != 2 {
		t.Fatalf("n=%d stopped=%v; break did not stop the read", n, stopped)
	}
}

// A query error arrives once, with a zero row, after the rows before it.
func TestSeqYieldsTheErrorOnce(t *testing.T) {
	boom := errors.New("boom")
	var stopped bool
	var rows, errs int
	for v, err := range runtime.Seq(eachOf([]int{7}, boom, &stopped)) {
		if err != nil {
			if err != boom || v != 0 {
				t.Fatalf("yielded (%d, %v), want (0, boom)", v, err)
			}
			errs++
			continue
		}
		rows++
	}
	if rows != 1 || errs != 1 {
		t.Fatalf("rows=%d errs=%d, want 1 and 1", rows, errs)
	}
}
