// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgsplice

import (
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

var p = Punctuation{And: `{"$and":[`, Or: `{"$or":[`, Sep: ",", End: "]}", Empty: "{}"}

func frag(op, col uint32) Frag {
	if op == 9 {
		return Frag{A: `{"c` + string(rune('0'+col)) + `":null}`}
	}
	return Frag{A: `{"c` + string(rune('0'+col)) + `":`, B: `}`, Arg: true}
}

func TestSpliceNumbersHolesInOutputOrder(t *testing.T) {
	toks := []runtime.Tok{
		runtime.MakeLeaf(1, 0),
		runtime.MakeLeaf(1, 1), runtime.MakeLeaf(9, 2), runtime.MakeGroup(runtime.KOr, 2),
		runtime.MakeOrder(runtime.Desc, 0),
		runtime.MakeLeaf(1, 3),
	}
	f, n, err := Splice(toks, frag, p, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"$and":[{"c0":{"$arg":3}},{"$or":[{"c1":{"$arg":4}},{"c2":null}]},{"c3":{"$arg":5}}]}`
	if f != want || n != 3 {
		t.Fatalf("got %s (%d)\nwant %s", f, n, want)
	}
	var v any
	if err := json.Unmarshal([]byte(f), &v); err != nil {
		t.Fatal(err)
	}
	if f, n, _ := Splice(nil, frag, p, 1); f != "{}" || n != 0 {
		t.Error("no predicates is the empty filter")
	}
	if f, _, _ := Splice(toks[:1], frag, p, 1); f != `{"c0":{"$arg":1}}` {
		t.Error("one predicate is itself, not a one-member $and:", f)
	}
	one := []runtime.Tok{runtime.MakeLeaf(1, 0), runtime.MakeGroup(runtime.KAnd, 1)}
	if f, _, _ := Splice(one, frag, p, 1); f != `{"c0":{"$arg":1}}` {
		t.Error("a group of one is its member:", f)
	}
}

// A stream that does not reduce must not become a filter that matches more.
func TestSpliceRefusesAMalformedStream(t *testing.T) {
	for _, toks := range [][]runtime.Tok{
		{runtime.MakeGroup(runtime.KAnd, 2)},
		{runtime.MakeLeaf(1, 0), runtime.MakeGroup(runtime.KOr, 0)},
		{runtime.MakeLeaf(1, 0), runtime.MakeGroup(runtime.KNot, 1)},
	} {
		if _, _, err := Splice(toks, frag, p, 1); !errors.Is(err, runtime.ErrMalformedStream) {
			t.Errorf("%v: %v", toks, err)
		}
	}
}

func TestSort(t *testing.T) {
	term := func(dir, col uint32) string {
		if dir == runtime.Desc {
			return `"c":-1`
		}
		return `"c":1`
	}
	toks := []runtime.Tok{runtime.MakeLeaf(1, 0), runtime.MakeOrder(runtime.Desc, 0), runtime.MakeOrder(runtime.Asc, 1)}
	if s := Sort(toks, term, "{", ",", "}", "D"); s != `{"c":-1,"c":1}` {
		t.Error(s)
	}
	if s := Sort(toks[:1], term, "{", ",", "}", "D"); s != "D" {
		t.Error("no order terms is the default")
	}
}

// The text matches are LITERAL: the caller's metacharacters match themselves.
func TestTextMatchesEscape(t *testing.T) {
	cases := []struct {
		re, in string
		want   bool
	}{
		{Prefix("a.c"), "a.cd", true},
		{Prefix("a.c"), "abcd", false},
		{Suffix("(x)"), "y(x)", true},
		{Suffix("(x)"), "(x)y", false},
		{Contains(".*"), "a.*b", true},
		{Contains(".*"), "ab", false},
		{Like("a%c"), "abbbc", true},
		{Like("a%c"), "abbbcd", false},
		{Like("a_c"), "abc", true},
		{Like("a_c"), "abbc", false},
		{Like("50%"), "50% off", true},
		{Like("a.b"), "axb", false},
		{Like("%"), "x\ny", true},
	}
	for _, c := range cases {
		if got := regexp.MustCompile(c.re).MatchString(c.in); got != c.want {
			t.Errorf("%q on %q: %v", c.re, c.in, got)
		}
	}
}
