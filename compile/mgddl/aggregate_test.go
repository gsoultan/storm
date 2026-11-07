// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgddl

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func agg(fn schema.AggFunc, arg schema.Expr) schema.Expr {
	return schema.Expr{Kind: schema.ExprAgg, Fn: string(fn), Args: []schema.Expr{arg}}
}

func col(name string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Col: name} }

func TestAggregationsPortOrSayWhyNot(t *testing.T) {
	s := users()
	tb := s.Tables[0]
	tb.Columns = append(tb.Columns,
		&schema.Column{Name: "n", Type: schema.Type{Name: schema.TypeInt8}},
		&schema.Column{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}},
		&schema.Column{Name: "ok", Type: schema.Type{Name: schema.TypeBool}})
	star := schema.Expr{Kind: schema.ExprStar}
	tb.Aggregates = []*schema.Aggregate{{Name: "Fine",
		By: []schema.GroupTerm{{Expr: col("mood"), As: "Mood"}},
		Terms: []schema.AggregateTerm{
			{Expr: agg(schema.AggCount, star), As: "A"},
			{Expr: agg(schema.AggCount, col("nick")), As: "B"},
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "count", Distinct: true, Args: []schema.Expr{col("nick")}}, As: "C"},
			{Expr: agg(schema.AggSum, col("n")), As: "D"},
			{Expr: agg(schema.AggAvg, col("n")), As: "E"},
			{Expr: agg(schema.AggMin, col("at")), As: "F"},
			{Expr: agg(schema.AggMax, col("n")), As: "G"},
		}}}
	if err := Check(s); err != nil {
		t.Fatalf("the subset must port: %v", err)
	}

	cond := &schema.Cond{}
	tb.Aggregates = []*schema.Aggregate{
		{Name: "Sets", Sets: &schema.GroupingSets{}},
		{Name: "Hav", Having: cond},
		{Name: "Joined", Joins: []schema.JoinTable{{}}},
		{Name: "Where", Where: cond},
		{Name: "Params", Params: []schema.Param{{}}},
		{Name: "ByExpr", By: []schema.GroupTerm{{Expr: schema.Expr{Kind: schema.ExprLit}, As: "X"},
			{Expr: col("tags"), As: "Tags"}}},
		{Name: "Terms", Terms: []schema.AggregateTerm{
			{Expr: schema.Expr{Kind: schema.ExprWindow}, As: "W"},
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "count", Args: []schema.Expr{star}, Filter: cond}, As: "Filt"},
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "count"}, As: "NoArg"},
			{Expr: agg(schema.AggSum, schema.Expr{Kind: schema.ExprLit}), As: "Lit"},
			{Expr: agg(schema.AggCount, col("tags")), As: "Arr"},
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum", Distinct: true, Args: []schema.Expr{col("n")}}, As: "SumD"},
			{Expr: agg(schema.AggSum, col("nick")), As: "SumText"},
			{Expr: agg(schema.AggMin, col("mood")), As: "MinEnum"},
			{Expr: agg(schema.AggMax, col("nick")), As: "MaxText"},
			{Expr: agg(schema.AggMin, col("ok")), As: "MinBool"},
			{Expr: agg(schema.AggFirst, col("n")), As: "First"},
		}},
	}
	err := Check(s)
	if err == nil {
		t.Fatal("nothing refused")
	}
	for _, want := range []string{
		"Sets uses grouping sets", "Hav uses HAVING", "Joined uses a join", "Where uses a declared WHERE",
		"Params uses parameters", "grouping by an expression (X)", "grouping by an array or json column (Tags)",
		"users.Terms: W: a window", "Filt: FILTER", "NoArg: an aggregate of other than one argument",
		"Lit: an aggregate over an expression", "Arr: an aggregate over an array",
		"SumD: DISTINCT other than in count", "SumText: sum of a text column", "MinEnum: min of an enum",
		"MaxText: max of text", "MinBool: min of a bool column", "First: the aggregate",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q\n%v", want, err)
		}
	}
}
