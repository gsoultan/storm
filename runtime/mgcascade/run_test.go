// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgcascade

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/bsondec"
)

// fake answers a find by the collection it names, with rows of raw values in
// the projection's order, and records every command.
type fake struct {
	runtime.Executor // Batch: unused
	rows             map[string][][][]byte
	found            map[string]bool
	fail             string
	log              []string
	args             [][]any
}

func (f *fake) Query(_ context.Context, cmd string, args []any) (runtime.Rows, error) {
	f.log = append(f.log, cmd)
	f.args = append(f.args, args)
	if f.fail != "" && strings.Contains(cmd, f.fail) {
		return nil, errors.New("boom")
	}
	coll := cmd[len(`{"find":"`):]
	coll = coll[:strings.IndexByte(coll, '"')]
	if strings.Contains(cmd, `"limit":1`) {
		if f.found[coll] {
			return &rows{data: [][][]byte{{[]byte("x")}}}, nil
		}
		return &rows{}, nil
	}
	return &rows{data: f.rows[coll]}, nil
}

func (f *fake) Exec(_ context.Context, cmd string, args []any) (int64, error) {
	f.log = append(f.log, cmd)
	f.args = append(f.args, args)
	if f.fail != "" && strings.Contains(cmd, f.fail) {
		return 0, errors.New("boom")
	}
	return 1, nil
}

func (f *fake) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, errors.New("unused")
}

type rows struct {
	data [][][]byte
	i    int
}

func (r *rows) Next() bool          { r.i++; return r.i <= len(r.data) }
func (r *rows) RawValues() [][]byte { return r.data[r.i-1] }
func (r *rows) Close()              {}
func (r *rows) Err() error          { return nil }

// tx and pool tell Delete which atomicity it has.
type tx struct {
	*fake
	done string
}

func (t *tx) Commit(context.Context) error   { t.done = "commit"; return nil }
func (t *tx) Rollback(context.Context) error { t.done = "rollback"; return nil }

type pool struct {
	*fake
	tx *tx
}

func (p *pool) StartTx(context.Context) (runtime.Tx, error) { return p.tx, nil }

// plan: authors ← books (cascade) ← volumes (restrict); authors ← volumes
// (set null, composite for the test's sake).
func plan() *Plan {
	return &Plan{Nodes: []Node{
		{Coll: "authors", Edges: []Edge{
			{Child: 1, Name: "books.author_id", From: []string{"author_id"}, To: []string{"_id"}, Action: Cascade},
			{Child: 2, Name: "volumes.donor", From: []string{"d1", "d2"}, To: []string{"_id", "k"}, Action: SetNull, Set: `[{"$set":{"d1":null,"d2":null}}]`},
		}},
		{Coll: "books", Edges: []Edge{
			{Child: 2, Name: "volumes.book_id", From: []string{"book_id"}, To: []string{"_id"}, Action: Restrict},
		}},
		{Coll: "volumes"},
	}}
}

const del = `{"delete":"authors","deletes":[{"limit":1,"q":{"_id":{"$arg":1}}}]}`

func TestDeleteCascadesNullsAndDeletesLast(t *testing.T) {
	f := &fake{rows: map[string][][][]byte{
		"authors": {{[]byte("a1"), []byte("k1")}, {[]byte("a1"), []byte("k1")}, {[]byte("a2"), nil}},
		"books":   {{[]byte("b1")}},
	}}
	n, err := Delete(context.Background(), f, plan(), `{"_id":{"$arg":1}}`, []any{"a"}, del)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	want := []string{
		`{"find":"authors","projection":{"_id":1,"k":1},"filter":{"_id":{"$arg":1}}}`,
		`{"find":"books","projection":{"_id":1},"filter":{"author_id":{"$in":{"$arg":1}}}}`,
		`{"find":"volumes","limit":1,"projection":{"_id":1},"filter":{"book_id":{"$in":{"$arg":1}}}}`,
		`{"delete":"books","deletes":[{"limit":0,"q":{"author_id":{"$in":{"$arg":1}}}}]}`,
		`{"update":"volumes","updates":[{"multi":true,"q":{"$or":[{"d1":{"$arg":1},"d2":{"$arg":2}}]},"u":[{"$set":{"d1":null,"d2":null}}]}]}`,
		del,
	}
	if strings.Join(f.log, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s", strings.Join(f.log, "\n"))
	}
	// a1 twice is one value; a2 is one; the composite edge skips a2, whose
	// k is missing — a NULL references nothing.
	if in := f.args[1][0].([]bsondec.Raw); len(in) != 2 || string(in[0]) != "a1" || string(in[1]) != "a2" {
		t.Errorf("distinct values: %q", in)
	}
	if a := f.args[4]; len(a) != 2 || string(a[0].(bsondec.Raw)) != "a1" || string(a[1].(bsondec.Raw)) != "k1" {
		t.Errorf("composite match: %q", a)
	}
}

func TestRestrictRefusesBeforeAnythingChanges(t *testing.T) {
	f := &fake{
		rows:  map[string][][][]byte{"books": {{[]byte("b1")}}},
		found: map[string]bool{"volumes": true},
	}
	p := &Plan{Nodes: []Node{plan().Nodes[1], {Coll: "volumes"}}}
	p.Nodes[0].Edges = []Edge{{Child: 1, Name: "volumes.book_id", From: []string{"book_id"}, To: []string{"_id"}, Action: Restrict}}
	_, err := Delete(context.Background(), f, p, `{}`, nil, `{"delete":"books"}`)
	if !errors.Is(err, runtime.ErrForeignKeyViolation) || !strings.Contains(err.Error(), "volumes.book_id") {
		t.Fatalf("restrict: %v", err)
	}
	for _, c := range f.log {
		if !strings.HasPrefix(c, `{"find"`) {
			t.Errorf("a refused delete ran %s", c)
		}
	}
}

func TestDeleteJoinsOrOpensATransaction(t *testing.T) {
	f := &fake{}
	in := &tx{fake: f}
	if _, err := Delete(context.Background(), in, plan(), `{}`, nil, del); err != nil || in.done != "" {
		t.Fatalf("a caller's transaction is the caller's to end: %q %v", in.done, err)
	}
	p := &pool{fake: &fake{}, tx: &tx{fake: &fake{}}}
	if _, err := Delete(context.Background(), p, plan(), `{}`, nil, del); err != nil || p.tx.done != "commit" {
		t.Fatalf("a pool's: %q %v", p.tx.done, err)
	}
	if len(p.fake.log) != 0 || len(p.tx.fake.log) == 0 {
		t.Error("the steps run inside the transaction")
	}
	bad := &pool{fake: &fake{}, tx: &tx{fake: &fake{fail: `"delete":"authors"`}}}
	if _, err := Delete(context.Background(), bad, plan(), `{}`, nil, del); err == nil || bad.tx.done != "rollback" {
		t.Fatalf("a failure rolls back: %q %v", bad.tx.done, err)
	}
}

func TestEveryStepsFailureIsReturned(t *testing.T) {
	data := map[string][][][]byte{"authors": {{[]byte("a"), []byte("k")}}, "books": {{[]byte("b")}}}
	for _, at := range []string{`"find":"authors"`, `"find":"books"`, `"find":"volumes"`, `"delete":"books"`, `"update":"volumes"`} {
		f := &fake{rows: data, fail: at}
		if _, err := Delete(context.Background(), f, plan(), `{}`, nil, del); err == nil {
			t.Errorf("%s failed silently", at)
		}
	}
}

func TestACycleStops(t *testing.T) {
	f := &fake{rows: map[string][][][]byte{"nodes": {{[]byte("n")}}}}
	p := &Plan{Nodes: []Node{{Coll: "nodes", Edges: []Edge{{Child: 0, Name: "nodes.parent", From: []string{"parent"}, To: []string{"_id"}, Action: Cascade}}}}}
	if _, err := Delete(context.Background(), f, p, `{}`, nil, `{}`); !errors.Is(err, ErrTooDeep) {
		t.Fatal(err)
	}
}

func TestSplitAndQuote(t *testing.T) {
	ts := make([][]bsondec.Raw, 2*chunk+1)
	for i := range ts {
		ts[i] = []bsondec.Raw{bsondec.Raw(strconv.Itoa(i))}
	}
	if s := split(ts); len(s) != 3 || len(s[0]) != chunk || len(s[2]) != 1 {
		t.Errorf("split: %d", len(s))
	}
	if split(nil) != nil {
		t.Error("nothing to split")
	}
	if quote(`a"b\c`) != `"a\"b\\c"` || quote("ab") != `"ab"` {
		t.Error("quote")
	}
}
