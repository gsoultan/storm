// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// args rewrites the arguments of storm's own types into ones caduceus
// encodes, leaving every other argument as it is. The slice is copied only
// when one needs rewriting, so a statement of plain arguments costs nothing.
//
// Each becomes the text PostgreSQL reads for its type, which caduceus sends
// as text for the parameter type the server described: numeric, interval,
// time and tstzrange accept their text whatever the client's settings.
func args(in []any) []any {
	var out []any
	for i, a := range in {
		v, ok := arg(a)
		if !ok {
			continue
		}
		if out == nil {
			out = append(make([]any, 0, len(in)), in...)
		}
		out[i] = v
	}
	if out == nil {
		return in
	}
	return out
}

// arg is one argument rewritten, and whether it needed it.
func arg(a any) (any, bool) {
	switch v := a.(type) {
	case runtime.Decimal:
		return v.String(), true
	case *runtime.Decimal:
		if v == nil {
			return nil, true
		}
		return v.String(), true
	case []runtime.Decimal:
		if v == nil {
			return nil, true
		}
		out := make([]string, len(v))
		for i := range v {
			out[i] = v[i].String()
		}
		return out, true
	case runtime.Interval:
		return intervalText(v), true
	case *runtime.Interval:
		if v == nil {
			return nil, true
		}
		return intervalText(*v), true
	case runtime.TimeOfDay:
		return v.String(), true
	case *runtime.TimeOfDay:
		if v == nil {
			return nil, true
		}
		return v.String(), true
	case runtime.TstzRange:
		return rangeText(v), true
	case *runtime.TstzRange:
		if v == nil {
			return nil, true
		}
		return rangeText(*v), true
	case runtime.JSON:
		if v == nil {
			return nil, true
		}
		return json.RawMessage(v), true
	}
	return nil, false
}

// intervalText writes the three fields apart, as PostgreSQL keeps them: a
// month and a day have no fixed length, so none is folded into another.
func intervalText(iv runtime.Interval) string {
	return strconv.Itoa(int(iv.Months)) + " mons " + strconv.Itoa(int(iv.Days)) + " days " +
		strconv.FormatInt(iv.Micros, 10) + " microseconds"
}

// rangeTime is the layout a bound is written in: to the nanosecond, with its
// offset, which PostgreSQL rounds to the microsecond it keeps.
const rangeTime = "2006-01-02 15:04:05.999999999Z07:00"

// rangeText writes a tstzrange literal, such as ["2026-01-01 09:00:00Z",).
func rangeText(r runtime.TstzRange) string {
	if r.Empty {
		return "empty"
	}
	var b strings.Builder
	if r.LowerInc && !r.LowerInf {
		b.WriteByte('[')
	} else {
		b.WriteByte('(')
	}
	if !r.LowerInf {
		writeBound(&b, r.Lower)
	}
	b.WriteByte(',')
	if !r.UpperInf {
		writeBound(&b, r.Upper)
	}
	if r.UpperInc && !r.UpperInf {
		b.WriteByte(']')
	} else {
		b.WriteByte(')')
	}
	return b.String()
}

func writeBound(b *strings.Builder, t time.Time) {
	b.WriteByte('"')
	b.WriteString(t.Format(rangeTime))
	b.WriteByte('"')
}
