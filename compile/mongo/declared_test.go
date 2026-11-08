// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

import (
	"math"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

var (
	tText = schema.Type{Name: schema.TypeText}
	tInt  = schema.Type{Name: schema.TypeInt4}
	tNum  = schema.Type{Name: schema.TypeNumeric}
	tUUID = schema.Type{Name: schema.TypeUUID}
)

func ecol(tbl, c string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Tbl: tbl, Col: c} }

func elit(l schema.Literal) schema.Expr { return schema.Expr{Kind: schema.ExprLit, Lit: l} }

func cmp(op schema.CmpOp, l, r schema.Expr) schema.Cond {
	return schema.Cond{Kind: schema.CondCmp, Op: op, Left: l, Right: r}
}

func shop() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{
		{Name: "orders", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
			{Name: "id", Type: tUUID, NotNull: true},
			{Name: "customer_id", Type: tUUID},
			{Name: "note", Type: tText},
			{Name: "qty", Type: tInt},
			{Name: "total", Type: tNum},
			{Name: "paid", Type: schema.Type{Name: schema.TypeBool}},
			{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}},
			{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}},
			{Name: "at", Type: schema.Type{Name: schema.TypeTimestamptz}},
		}},
		{Name: "customers", PrimaryKey: []string{"id"}, SoftDelete: "gone", Columns: []*schema.Column{
			{Name: "id", Type: tUUID, NotNull: true},
			{Name: "email", Type: tText},
			{Name: "n", Type: tInt},
			{Name: "gone", Type: schema.Type{Name: schema.TypeTimestamptz}},
		}},
		{Name: "lines", PrimaryKey: []string{"id"}, Columns: []*schema.Column{{Name: "id", Type: tUUID}},
			Relations: []*schema.Relation{{Field: "Order", Target: "orders", Owner: true, Column: "order_id", Document: Embed}}},
		{Name: "links", Generated: true},
	}}
}

func TestFilterSaysWhatSQLSays(t *testing.T) {
	o := shop().Tables[0]
	resolve := func(tbl, col string) (string, *schema.Column, error) {
		c := o.Column(col)
		if c == nil {
			return "", nil, errUnknown
		}
		return Field(o, col), c, nil
	}
	param := func(i int) string { return Arg(i + 1) }
	for _, c := range []struct {
		cond schema.Cond
		want string
	}{
		{cmp(schema.OpEq, ecol("", "note"), elit(schema.Literal{Kind: schema.TypeText, S: "$x"})), `{"note":{"$eq":"$x"}}`},
		{cmp(schema.OpNe, ecol("", "qty"), elit(schema.Literal{Kind: schema.TypeInt8, I: 3})), `{"qty":{"$nin":[3,null]}}`},
		{cmp(schema.OpLt, elit(schema.Literal{Kind: schema.TypeInt4, I: 3}), ecol("", "qty")), `{"qty":{"$gt":3}}`},
		{cmp(schema.OpGte, ecol("", "qty"), elit(schema.Literal{Kind: schema.TypeFloat8, F: 1.5})), `{"qty":{"$gte":1.5}}`},
		{cmp(schema.OpEq, ecol("", "paid"), elit(schema.Literal{Kind: schema.TypeBool, B: true})), `{"paid":{"$eq":true}}`},
		{cmp(schema.OpEq, ecol("", "mood"), elit(schema.Literal{Kind: schema.TypeText, S: "ok"})), `{"mood":{"$eq":"ok"}}`},
		{cmp(schema.OpEq, ecol("", "id"), schema.Expr{Kind: schema.ExprParam, Param: 1, Type: tUUID}), `{"_id":{"$eq":{"$arg":2}}}`},
		{schema.Cond{Kind: schema.CondOr, Args: []schema.Cond{
			{Kind: schema.CondIsNull, Left: ecol("", "note")},
			{Kind: schema.CondIsNotNull, Left: ecol("", "qty")}}}, `{"$or":[{"note":null},{"qty":{"$ne":null}}]}`},
	} {
		got, err := Filter(c.cond, resolve, param)
		if err != nil || got != c.want {
			t.Errorf("got %s %v, want %s", got, err, c.want)
		}
	}
	for _, c := range []struct {
		cond schema.Cond
		want string
	}{
		{schema.Cond{Kind: schema.CondNot}, "NOT"},
		{schema.Cond{Kind: schema.CondIsNull, Left: elit(schema.Literal{})}, "IS NULL over an expression"},
		{schema.Cond{Kind: schema.CondIsNull, Left: ecol("", "nope")}, "unknown"},
		{schema.Cond{Kind: schema.CondAnd, Args: []schema.Cond{{Kind: schema.CondNot}}}, "NOT"},
		{cmp(schema.OpEq, ecol("", "qty"), ecol("", "qty")), "a comparison other than"},
		{cmp(schema.OpEq, ecol("", "nope"), elit(schema.Literal{})), "unknown"},
		{cmp(schema.OpEq, ecol("", "tags"), elit(schema.Literal{Kind: schema.TypeText})), "element-wise"},
		{cmp(schema.OpEq, ecol("", "qty"), schema.Expr{Kind: schema.ExprParam, Type: tText}), "another type"},
		{cmp(schema.OpEq, ecol("", "qty"), schema.Expr{Kind: schema.ExprFunc}), "an expression"},
		{cmp(schema.OpEq, ecol("", "qty"), elit(schema.Literal{Kind: schema.TypeText})), "with a text literal"},
		{cmp(schema.OpEq, ecol("", "total"), elit(schema.Literal{Kind: schema.TypeFloat8, F: 1})), "a decimal with a float"},
		{cmp(schema.OpEq, ecol("", "qty"), elit(schema.Literal{Kind: schema.TypeFloat8, F: math.NaN()})), "NaN"},
		{cmp(schema.OpEq, ecol("", "total"), elit(schema.Literal{Kind: schema.TypeNumeric, S: "1"})), "use a parameter"},
		{cmp(schema.OpLt, ecol("", "note"), elit(schema.Literal{Kind: schema.TypeText})), "by collation"},
		{cmp(schema.OpLt, ecol("", "id"), schema.Expr{Kind: schema.ExprParam, Type: tUUID}), "ordering id"},
	} {
		if _, err := Filter(c.cond, resolve, param); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	if _, err := Filter(cmp(schema.OpEq, ecol("", "qty"), schema.Expr{Kind: schema.ExprParam, Type: tInt}), resolve, nil); err == nil {
		t.Error("a parameter where none is declared")
	}
}

var errUnknown = errorString("unknown column")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestProjectLiteral(t *testing.T) {
	for _, c := range []struct {
		l    schema.Literal
		want string
	}{
		{schema.Literal{Kind: schema.TypeText, S: "$a"}, `{"$literal":"$a"}`},
		{schema.Literal{Kind: schema.TypeInt4, I: 2}, `{"$toInt":2}`},
		{schema.Literal{Kind: schema.TypeInt8, I: 2}, `{"$toLong":2}`},
		{schema.Literal{Kind: schema.TypeFloat8, F: 0.5}, `{"$toDouble":0.5}`},
		{schema.Literal{Kind: schema.TypeBool, B: true}, `true`},
		{schema.Literal{Kind: schema.TypeNumeric, S: "1.50"}, `{"$toDecimal":"1.50"}`},
		{schema.Literal{Kind: schema.TypeTimestamptz, S: "2001-02-03T04:05:06Z"}, `{"$toDate":"2001-02-03T04:05:06Z"}`},
	} {
		if got, err := ProjectLiteral(c.l); err != nil || got != c.want {
			t.Errorf("got %s %v, want %s", got, err, c.want)
		}
	}
	for _, l := range []schema.Literal{{Kind: schema.TypeFloat8, F: math.Inf(1)}, {Kind: schema.TypeBytea}} {
		if _, err := ProjectLiteral(l); err == nil {
			t.Errorf("%v must be refused", l)
		}
	}
}

func onEq(l, r schema.Expr) schema.Cond { return cmp(schema.OpEq, l, r) }

func TestJoinLowersOrSaysWhy(t *testing.T) {
	s := shop()
	o := s.Tables[0]
	j := &schema.Join{Name: "WithCustomer",
		Tables: []schema.JoinTable{{Kind: schema.JoinLeft, Table: "customers", Alias: "c",
			On: schema.Cond{Kind: schema.CondAnd, Args: []schema.Cond{onEq(ecol("orders", "customer_id"), ecol("c", "id"))}}}},
		Select:  []schema.JoinCol{{Expr: ecol("orders", "note"), As: "Note"}, {Expr: ecol("c", "email"), As: "Email"}},
		Where:   &schema.Cond{Kind: schema.CondIsNotNull, Left: ecol("c", "email")},
		OrderBy: []schema.JoinOrder{{Expr: ecol("c", "email"), Desc: true}},
	}
	got, err := Join(s, o, j)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"$lookup":{"from":"customers","let":{"v0":{"$ifNull":["$customer_id",null]}},"pipeline":[{"$match":{"$expr":{"$and":[{"$ne":["$$v0",null]},{"$eq":["$_id","$$v0"]}]}}},{"$match":{"gone":null}}],"as":"_j0"}}`,
		`{"$unwind":{"path":"$_j0","preserveNullAndEmptyArrays":true}}`,
		`{"$match":{"_j0.email":{"$ne":null}}}`,
		`{"$sort":{"_null0":-1,"_j0.email":-1}}`,
		`{"$project":{"Note":"$note","Email":"$_j0.email"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s in\n%s", want, got)
		}
	}
	for want, edit := range map[string]func(j *schema.Join){
		"a CTE":                     func(j *schema.Join) { j.CTEs = []schema.CTE{{}} },
		"json array elements":       func(j *schema.Join) { j.Tables[0].Kind = schema.JoinElements },
		"other than a table":        func(j *schema.Join) { j.Tables[0].Table = "nope" },
		"embedded":                  func(j *schema.Join) { j.Tables[0].Table = "lines" },
		"generated join table":      func(j *schema.Join) { j.Tables[0].Table = "links" },
		"the alias orders twice":    func(j *schema.Join) { j.Tables[0].Alias = "orders" },
		"on other than equal":       func(j *schema.Join) { j.Tables[0].On.Args[0].Op = schema.OpLt },
		"no column c.nope":          func(j *schema.Join) { j.Tables[0].On.Args[0].Right.Col = "nope" },
		"no column orders.nope":     func(j *schema.Join) { j.Tables[0].On.Args[0].Left.Col = "nope" },
		"the alias x, not yet":      func(j *schema.Join) { j.Tables[0].On.Args[0].Left.Tbl = "x" },
		"which SQL would cast":      func(j *schema.Join) { j.Tables[0].On.Args[0].Left.Col = "qty" },
		"NOT":                       func(j *schema.Join) { j.Where = &schema.Cond{Kind: schema.CondNot} },
		"an order by an expression": func(j *schema.Join) { j.OrderBy[0].Expr.Kind = schema.ExprFunc },
		"an order by an array":      func(j *schema.Join) { j.OrderBy[0].Expr = ecol("", "tags") },
		"no column c.zz":            func(j *schema.Join) { j.OrderBy[0].Expr.Col = "zz" },
		"Email, an expression":      func(j *schema.Join) { j.Select[1].Expr.Kind = schema.ExprFunc },
		"no column c.yy":            func(j *schema.Join) { j.Select[1].Expr.Col = "yy" },
	} {
		jj := *j
		jj.Tables = []schema.JoinTable{j.Tables[0]}
		jj.Tables[0].On.Args = append([]schema.Cond(nil), j.Tables[0].On.Args...)
		jj.Select = append([]schema.JoinCol(nil), j.Select...)
		jj.OrderBy = append([]schema.JoinOrder(nil), j.OrderBy...)
		edit(&jj)
		if _, err := Join(s, o, &jj); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestUnionLowersOrSaysWhy(t *testing.T) {
	s := shop()
	u := &schema.Union{Name: "Feed",
		Params: []schema.Param{{Name: "N", Type: tInt}},
		Cols:   []schema.UnionCol{{As: "Text", Type: tText, Nullable: true}, {As: "N", Type: schema.Type{Name: schema.TypeInt8}}},
		Branches: []schema.UnionBranch{
			{Table: "orders", Exprs: []schema.Expr{ecol("", "note"), elit(schema.Literal{Kind: schema.TypeInt8, I: 1})}},
			{Table: "customers", Exprs: []schema.Expr{ecol("customers", "email"), ecol("", "n")},
				Where: &schema.Cond{Kind: schema.CondCmp, Op: schema.OpGt, Left: ecol("", "n"), Right: schema.Expr{Kind: schema.ExprParam, Type: tInt}}},
		},
		OrderBy: []schema.UnionOrder{{Col: "Text", NullsFirst: new(bool)}},
	}
	got, err := Union(s, u)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"aggregate":"orders","cursor":{},"pipeline":[{"$match":{}},{"$project":{"Text":{"$ifNull":["$note",null]},"N":{"$toLong":1}}},` +
		`{"$unionWith":{"coll":"customers","pipeline":[{"$match":{"$and":[{"gone":null},{"n":{"$gt":{"$arg":1}}}]}},{"$project":{"Text":{"$ifNull":["$email",null]},"N":{"$ifNull":["$n",null]}}}]}},` +
		`{"$addFields":{"_null0":{"$cond":[{"$eq":[{"$ifNull":["$Text",null]},null]},1,0]}}},{"$sort":{"_null0":1,"Text":1}},{"$limit":{"$arg":2}}]}`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for want, edit := range map[string]func(u *schema.Union){
		"no branches":         func(u *schema.Union) { u.Branches = nil },
		"not a table":         func(u *schema.Union) { u.Branches[0].Table = "nope" },
		"embedded":            func(u *schema.Union) { u.Branches[1].Table = "lines" },
		"another width":       func(u *schema.Union) { u.Branches[1].Exprs = u.Branches[1].Exprs[:1] },
		"no column":           func(u *schema.Union) { u.Branches[1].Exprs[0].Col = "nope" },
		"of another type":     func(u *schema.Union) { u.Branches[1].Exprs[0].Col = "n" },
		"Text from orders: a": func(u *schema.Union) { u.Branches[0].Exprs[0] = elit(schema.Literal{Kind: schema.TypeBytea}) },
		"an expression":       func(u *schema.Union) { u.Branches[0].Exprs[0].Kind = schema.ExprFunc },
		"NOT":                 func(u *schema.Union) { u.Branches[1].Where = &schema.Cond{Kind: schema.CondNot} },
		"not an output":       func(u *schema.Union) { u.OrderBy[0].Col = "Nope" },
		"an array or json": func(u *schema.Union) {
			u.Cols[0].Type = schema.Type{Name: schema.TypeJSONB}
			u.Branches = u.Branches[:1]
			u.Branches[0].Exprs[0] = elit(schema.Literal{Kind: schema.TypeText})
		},
	} {
		uu := *u
		uu.Cols = append([]schema.UnionCol(nil), u.Cols...)
		uu.OrderBy = append([]schema.UnionOrder(nil), u.OrderBy...)
		uu.Branches = nil
		for _, b := range u.Branches {
			b.Exprs = append([]schema.Expr(nil), b.Exprs...)
			uu.Branches = append(uu.Branches, b)
		}
		edit(&uu)
		if _, err := Union(s, &uu); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	u.Distinct = true
	if got, err := Union(s, u); err != nil || !strings.Contains(got, `{"$group":{"_id":{"Text":"$Text","N":"$N"}}},{"$project":{"Text":"$_id.Text","N":"$_id.N"}}`) {
		t.Errorf("distinct: %s %v", got, err)
	}
	for _, c := range []struct {
		from, to schema.Type
		ok       bool
	}{
		{tInt, schema.Type{Name: schema.TypeInt8}, true},
		{schema.Type{Name: schema.TypeInt2}, tInt, true},
		{schema.Type{Name: schema.TypeFloat4}, schema.Type{Name: schema.TypeFloat8}, true},
		{schema.Type{Name: schema.TypeVarchar}, tText, true},
		{tInt, tNum, false},
		{tText, schema.Type{Name: schema.TypeText, Array: true}, false},
	} {
		if widens(c.from, c.to) != c.ok {
			t.Errorf("widens(%v, %v) != %v", c.from, c.to, c.ok)
		}
	}
}
