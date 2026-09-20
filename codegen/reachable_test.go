package codegen

// Every column kind a model can declare must be reachable from a builder, or
// be listed here with the reason it is not.
//
// Three of v0.16.0's findings had the same shape: the model could DESCRIBE the
// thing and no builder could say it. bytea was the plainest — it sat in the
// Eq exclusion list beside jsonb and arrays, whose equality genuinely
// surprises, and its own does not: byte for byte is exactly what a hash lookup
// means. Excluding it made every digest-keyed table unreachable from a
// builder, and refresh tokens, one-time tokens and session cookies are all
// looked up by hash and by nothing else.
//
// Nothing could have caught that, because the exclusion list was a switch and
// a switch has no opinion about what is missing from it. This inverts the
// default: a kind is expected to be addressable, and one that is not has to be
// argued for HERE, in a table a reviewer reads.

import (
	"testing"

	"github.com/gsoultan/storm/schema"
)

// noEqBecause is every kind a caller cannot compare, and why.
//
// Each reason is about a comparison that SURPRISES — not about a type being
// awkward. That distinction is the one bytea was on the wrong side of.
var noEqBecause = map[kind]string{
	kindTSVector: "equality asks whether two documents have identical lexeme " +
		"vectors, which nobody means; the match operators are the whole reason " +
		"the column exists",
	kindJSONB: "whole-document equality, which is almost never what a caller " +
		"means; filtering jsonb needs ->> and @>",
	kindTextArray:    "array equality is order-sensitive in a way almost nobody means",
	kindUUIDArray:    "array equality is order-sensitive in a way almost nobody means",
	kindInt8Array:    "array equality is order-sensitive in a way almost nobody means",
	kindInt4Array:    "array equality is order-sensitive in a way almost nobody means",
	kindDecimalArray: "array equality is order-sensitive in a way almost nobody means",
	kindInterval: "interval equality compares normalised values ('24:00' = '1 day'), " +
		"which surprises in both directions",
}

var kindNames = map[kind]string{
	kindTSVector: "tsvector", kindTstzRange: "tstzrange", kindBool: "bool",
	kindInt2: "int2", kindInt4: "int4", kindInt8: "int8",
	kindFloat4: "float4", kindFloat8: "float8", kindText: "text",
	kindBytes: "bytea", kindUUID: "uuid", kindTimestamptz: "timestamptz",
	kindNumeric: "numeric", kindJSONB: "jsonb", kindTextArray: "text[]",
	kindUUIDArray: "uuid[]", kindDate: "date", kindInterval: "interval",
	kindTimeOfDay: "time", kindInet: "inet", kindInt8Array: "int8[]",
	kindInt4Array: "int4[]", kindDecimalArray: "numeric[]",
}

func TestEveryKindIsComparableOrAccountedFor(t *testing.T) {
	for k := kindTSVector; k <= kindDecimalArray; k++ {
		name, known := kindNames[k]
		if !known {
			t.Errorf("kind %d has no name here — a kind was added and this table "+
				"was not updated, so nothing is checking whether it is reachable", int(k))
			continue
		}
		// A plain NOT NULL column of that kind: the question is about the KIND,
		// so nothing else about the column may influence the answer.
		c := &schema.Column{Name: "c", NotNull: true}
		has := opApplies("Eq", k, c)
		why, listed := noEqBecause[k]

		switch {
		case has && listed:
			t.Errorf("%s is comparable AND listed as not comparable (%q) — one of "+
				"the two is stale", name, why)
		case !has && !listed:
			t.Errorf("%s cannot be compared and no reason is recorded. A kind a "+
				"model can declare and no predicate can address is a table "+
				"nothing can look a row up in — that is what happened to bytea. "+
				"Give it Eq, or add it to noEqBecause with the comparison that "+
				"surprises.", name)
		}
	}
}

// ...and the ones that ARE comparable stay comparable. The table above says
// what is excluded; this says the rest is not quietly joining it.
func TestTheEverydayKindsAreComparable(t *testing.T) {
	for _, k := range []kind{
		kindBool, kindInt2, kindInt4, kindInt8, kindFloat4, kindFloat8,
		kindText, kindUUID, kindTimestamptz, kindNumeric, kindDate,
		kindTimeOfDay, kindInet, kindTstzRange,
		// The one this test exists for.
		kindBytes,
	} {
		if !opApplies("Eq", k, &schema.Column{Name: "c", NotNull: true}) {
			t.Errorf("%s lost its Eq predicate — every row addressed by one is now "+
				"unreachable from a builder", kindNames[k])
		}
	}
}
