// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package library_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/gsoultan/storm/compile/mgexpr"
	"github.com/gsoultan/storm/examples/library/store/book"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
)

// The model's CHECK, enforced by the collection's validator.
func TestCheckConstraint(t *testing.T) {
	ctx := context.Background()
	b := book.Create()
	b.SetTitle("Empty")
	b.SetPages(0)
	b.SetPrice(runtime.Decimal{Unscaled: 1, Scale: 2})
	b.SetAuthorID(runtime.NewUUIDv7()) // a reference is not checked on insert
	if _, err := b.Insert(ctx, db); !errors.Is(err, runtime.ErrCheckViolation) {
		t.Fatalf("pages > 0: %v", err)
	}
	b.SetPages(1)
	b.SetPrice(runtime.Decimal{Unscaled: -1, Scale: 2})
	if _, err := b.Insert(ctx, db); !errors.Is(err, runtime.ErrCheckViolation) {
		t.Fatalf("price >= 0: %v", err)
	}
}

// TestTranslatedExpressionsAnswerAsSQLDoes holds compile/mgexpr to SQL's
// answers on a real server: each CHECK against a document SQL accepts or
// refuses — NULLs included, which is where a naive translation goes wrong —
// and each partial index against the documents SQL's WHERE would index.
func TestTranslatedExpressionsAnswerAsSQLDoes(t *testing.T) {
	ctx := context.Background()
	tb := &schema.Table{Name: "x", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
		{Name: "id", Type: schema.Type{Name: schema.TypeInt4}},
		{Name: "n", Type: schema.Type{Name: schema.TypeInt4}},
		{Name: "m", Type: schema.Type{Name: schema.TypeInt8}},
		{Name: "s", Type: schema.Type{Name: schema.TypeText}},
		{Name: "b", Type: schema.Type{Name: schema.TypeBool}},
		{Name: "d", Type: schema.Type{Name: schema.TypeNumeric}},
	}}
	cases := []struct {
		check string
		doc   string
		ok    bool
	}{
		{"n >= 0", `{"n":1}`, true},
		{"n >= 0", `{"n":-1}`, false},
		{"n >= 0", `{}`, true},
		{"n >= 0", `{"n":null}`, true},
		{"n >= 0 AND s <> ''", `{"n":-1}`, false},
		{"n >= 0 AND s <> ''", `{"s":""}`, false},
		{"n >= 0 AND s <> ''", `{}`, true},
		{"n >= 0 OR s <> ''", `{"n":-1}`, true},
		{"n >= 0 OR s <> ''", `{"n":-1,"s":""}`, false},
		{"NOT (n > 5)", `{"n":6}`, false},
		{"NOT (n > 5)", `{}`, true},
		{"n IS NOT NULL", `{}`, false},
		{"n IS NOT NULL", `{"n":null}`, false},
		{"n IS NOT NULL", `{"n":0}`, true},
		{"n IN (1, 2)", `{"n":3}`, false},
		{"n IN (1, 2)", `{"n":2}`, true},
		{"n NOT IN (1, 2)", `{"n":2}`, false},
		{"n NOT IN (1, NULL)", `{"n":3}`, true}, // UNKNOWN passes
		{"n BETWEEN 1 AND 3", `{"n":4}`, false},
		{"n BETWEEN 1 AND 3", `{"n":3}`, true},
		{"length(s) <= 3", `{"s":"héé"}`, true},
		{"length(s) <= 3", `{"s":"hééé"}`, false},
		{"s LIKE '%@%.%'", `{"s":"a@b.c"}`, true},
		{"s LIKE '%@%.%'", `{"s":"a@bc"}`, false},
		{"s LIKE 'a_c'", `{"s":"a\nc"}`, true},
		{"s = '$n'", `{"s":"$n","n":5}`, true},
		{"s = '$n'", `{"s":"5","n":5}`, false},
		{"n + m < 10", `{"n":5,"m":{"$numberLong":"5"}}`, false},
		{"n + m < 10", `{"n":5}`, true},
		{"m < 3000000000", `{"m":{"$numberLong":"3000000001"}}`, false},
		{"coalesce(n, 0) >= 0", `{"n":-1}`, false},
		{"coalesce(n, -1) >= 0", `{}`, false},
		{"abs(n) < 2", `{"n":-3}`, false},
		{"-n < 0", `{"n":1}`, true},
		{"b", `{"b":false}`, false},
		{"b", `{}`, true},
		{"b = true OR n = 1", `{"b":false,"n":1}`, true},
		{"d >= 0", `{"d":{"$arg":1}}`, false},
		{"d >= 1", `{"d":{"$arg":1}}`, false},
		{"d < 0", `{"d":{"$arg":1}}`, true},
	}
	for i, c := range cases {
		coll := "expr_check_" + strconv.Itoa(i)
		_, _ = db.Exec(ctx, `{"drop":"`+coll+`"}`, nil)
		t.Cleanup(func() { _, _ = db.Exec(ctx, `{"drop":"`+coll+`"}`, nil) })
		e, err := mgexpr.Check(tb, c.check)
		if err != nil {
			t.Errorf("%s: %v", c.check, err)
			continue
		}
		if _, err := db.Exec(ctx, `{"create":"`+coll+`","validator":{"$expr":`+e+`},"validationLevel":"strict","validationAction":"error"}`, nil); err != nil {
			t.Errorf("%s: create: %v", c.check, err)
			continue
		}
		_, err = db.Exec(ctx, `{"insert":"`+coll+`","documents":[`+c.doc+`]}`, []any{runtime.Decimal{Unscaled: -1, Scale: 2}})
		if (err == nil) != c.ok {
			t.Errorf("%s with %s: accepted=%v, SQL says %v (%v)", c.check, c.doc, err == nil, c.ok, err)
		}
	}
	parts := []struct {
		where string
		docs  []string
		in    int
	}{
		{"n > 0", []string{`{"n":1}`, `{"n":0}`, `{}`, `{"n":null}`}, 1},
		{"s IS NOT NULL", []string{`{"s":"a"}`, `{"s":null}`, `{}`}, 1},
		{"s IS NULL", []string{`{"s":"a"}`, `{"s":null}`, `{}`}, 2},
		{"n IN (1, 2) OR b", []string{`{"n":1}`, `{"n":3}`, `{"b":true}`, `{"b":false}`}, 2},
		{"n BETWEEN 1 AND 2 AND NOT b", []string{`{"n":1,"b":false}`, `{"n":1,"b":true}`, `{"n":1}`}, 1},
	}
	for i, p := range parts {
		coll := "expr_partial_" + strconv.Itoa(i)
		_, _ = db.Exec(ctx, `{"drop":"`+coll+`"}`, nil)
		t.Cleanup(func() { _, _ = db.Exec(ctx, `{"drop":"`+coll+`"}`, nil) })
		f, err := mgexpr.Partial(tb, p.where)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `{"createIndexes":"`+coll+`","indexes":[{"key":{"k":1},"name":"k","unique":true,"partialFilterExpression":`+f+`}]}`, nil); err != nil {
			t.Errorf("%s: %s: %v", p.where, f, err)
			continue
		}
		// Every doc shares k: the unique index admits one in-filter doc and
		// refuses the second.
		in := 0
		for _, d := range p.docs {
			if _, err := db.Exec(ctx, `{"insert":"`+coll+`","documents":[`+d[:len(d)-1]+map[bool]string{true: ``, false: `,`}[d == "{}"]+`"k":1}]}`, nil); err == nil {
				in++
			}
		}
		// in counts the docs admitted: all out-of-filter, plus one in-filter.
		if want := len(p.docs) - p.in + 1; in != want {
			t.Errorf("%s (%s): admitted %d, want %d", p.where, f, in, want)
		}
	}
}
