package mgexpr

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func table() *schema.Table {
	col := func(name, typ string) *schema.Column {
		return &schema.Column{Name: name, Type: schema.Type{Name: typ}}
	}
	mood := &schema.Column{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}}
	return &schema.Table{Name: "t", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
		col("id", schema.TypeUUID), col("n", schema.TypeInt4), col("m", schema.TypeInt8),
		col("f", schema.TypeFloat8), col("d", schema.TypeNumeric), col("s", schema.TypeText),
		col("v", schema.TypeVarchar), col("b", schema.TypeBool), col("at", schema.TypeTimestamptz),
		col("day", schema.TypeDate), mood,
		{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}},
		col("j", schema.TypeJSONB), col("ip", schema.TypeInet),
	}}
}

func TestCheckTranslates(t *testing.T) {
	for expr, want := range map[string]string{
		"n >= 0":                       `{"$ne":[{"$let":{"vars":{"a":{"$ifNull":["$n",null]},"b":0},"in":{"$cond":[{"$or":[{"$eq":["$$a",null]},{"$eq":["$$b",null]}]},null,{"$gte":["$$a","$$b"]}]}}},false]}`,
		"b":                            `{"$ne":[{"$ifNull":["$b",null]},false]}`,
		"s IS NOT NULL AND id IS NULL": `{"$ne":[{"$let":{"vars":{"v":[{"$ne":[{"$ifNull":["$s",null]},null]},{"$eq":[{"$ifNull":["$_id",null]},null]}]},"in":{"$cond":[{"$in":[false,"$$v"]},false,{"$cond":[{"$in":[null,"$$v"]},null,true]}]}}},false]}`,
		"s LIKE 'a.%'":                 `"regex":"^a\\..*$"`,
		"s NOT LIKE 'x\\%_'":           `{"$not":[{"$regexMatch":{"input":"$$a","regex":"^x%.$","options":"s"}}]}`,
		"m < 3000000000":               `{"$numberLong":"3000000000"}`,
		"f > .5 OR f < -2.":            `"b":0.5}`,
		"f between -.5 and 1":          `"b":-0.5}`,
		"length(v) <= 10":              `{"$strLenCP":"$$a"}`,
		"char_length(s) > 0":           `{"$strLenCP":"$$a"}`,
		"abs(n - m) * 2 + 1 < 10":      `{"$add":[{"$multiply":[{"$abs":{"$subtract":[{"$ifNull":["$n",null]},{"$ifNull":["$m",null]}]}},2]},1]}`,
		"coalesce(n, 0) <> -1":         `{"$ifNull":[{"$ifNull":["$n",null]},0]}`,
		"-n < 0":                       `{"$multiply":[-1,{"$ifNull":["$n",null]}]}`,
		"mood IN ('ok', 'fine')":       `{"$literal":"fine"}`,
		"s = '$x'":                     `{"$literal":"$x"}`,
		"d >= 0::numeric AND n = 1::int AND b = true::boolean AND s <> 'x'::text AND f > 1::double precision": `"$ne"`,
		`NOT ("n" = NULL)`:                `{"$not":["$$a"]}`,
		"at > at OR day = day OR id = id": `"$gt"`,
		"(n IS NOT NULL) IS NOT NULL":     `"$ne"`,
		"s = 'it''s'":                     `it's`,
		"v = 'x'::character varying":      `"$eq"`,
		"n NOT BETWEEN 1 AND 2":           `{"$not":["$$a"]}`,
	} {
		got, err := Check(table(), expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s:\n got %s\nwant %s", expr, got, want)
		}
	}
}

func TestCheckRefuses(t *testing.T) {
	for expr, want := range map[string]string{
		"n / 2 > 0":                           "division",
		"n % 2 = 0":                           "division",
		"s < 'b'":                             "ordering text",
		"mood > 'ok'":                         "ordering an enum",
		"b < true":                            "ordering a bool value",
		"d > 0.5":                             "a numeric column against a fractional literal",
		"d > f":                               "a numeric column against a fractional literal or a float",
		"n = 'x'":                             "comparing int with str",
		"at > '2020-01-01'":                   "comparing timestamptz with str",
		"lower(s) = 'x'":                      "the function lower()",
		"nope = 1":                            "nope, which is not a column of t",
		"tags IS NULL":                        "the array or json column tags",
		"ip IS NULL":                          "the inet column ip",
		"n":                                   "an int value where a condition belongs",
		"n AND b":                             "an int value where a condition belongs",
		"NOT n":                               "an int value where a condition belongs",
		"s IS TRUE":                           "IS other than",
		"n::text = 'x'":                       "a cast of a non-literal",
		"'x'::int = n":                        "the cast ::int",
		"s = 'x":                              "unterminated string",
		`"s = 1`:                              "unterminated quoted",
		"n = 1 ;":                             "the character ';'",
		"n = 1)":                              "unexpected )",
		"n = (1":                              "expected )",
		"n =":                                 "ends early",
		"n = ,":                               "unexpected ,",
		"n BETWEEN 1 OR 2":                    "expected and",
		"n IN 1":                              "expected (",
		"n IN (1,":                            "ends early",
		"n IN ('x')":                          "comparing int",
		"s LIKE s":                            "not a string literal",
		"n LIKE 'x'":                          "LIKE on an int value",
		"length(n) > 0":                       "length of anything",
		"abs(s) > 0":                          "abs of anything",
		"coalesce(n) = 1":                     "fewer than two",
		"coalesce(n, s) = 1":                  "comparing",
		"s + 1 = 2":                           "arithmetic on a text value",
		"n + 'x' = 1":                         "arithmetic on a str value",
		"d + 0.5 = 1":                         "fractional",
		"n BETWEEN s AND 2":                   "comparing",
		"n BETWEEN 1 AND s":                   "comparing",
		"n NOT IN (1, s)":                     "comparing",
		"n = 1::":                             "a cast with no type",
		"abs() = 1":                           "abs of anything",
		"coalesce(n, (1) = 1":                 "expected )",
		"n BETWEEN (1 AND 2":                  "an int value where",
		"n = abs(1 +)":                        "unexpected )",
		"(n = 1":                              "expected )",
		"length(s) = 1 AND (n BETWEEN 1 AND)": "unexpected )",
	} {
		_, err := Check(table(), expr)
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", expr, err, want)
		}
	}
}

func TestPartialTranslates(t *testing.T) {
	for expr, want := range map[string]string{
		"b":                         `{"b":true}`,
		"NOT b":                     `{"b":false}`,
		"s IS NULL":                 `{"s":null}`,
		"s IS NOT NULL":             `{"s":{"$type":"string"}}`,
		"n > 0":                     `{"n":{"$gt":0}}`,
		"0 < n":                     `{"n":{"$gt":0}}`,
		"5 >= m":                    `{"m":{"$lte":5}}`,
		"s = '$x'":                  `{"s":"$x"}`,
		"mood IN ('a', 'b')":        `{"mood":{"$in":["a","b"]}}`,
		"n IN (1, 2) OR b":          `{"$or":[{"n":{"$in":[1,2]}},{"b":true}]}`,
		"n BETWEEN 1 AND 2":         `{"$and":[{"n":{"$gte":1}},{"n":{"$lte":2}}]}`,
		"id IS NOT NULL AND n <= 3": `{"$and":[{"_id":{"$type":"binData"}},{"n":{"$lte":3}}]}`,
	} {
		got, err := Partial(table(), expr)
		if err != nil || got != want {
			t.Errorf("%s:\n got %s %v\nwant %s", expr, got, err, want)
		}
	}
	for _, expr := range []string{
		"n <> 1", "NOT (n = 1)", "n = NULL", "n IN (1, NULL)", "n IN (1, m)", "n = m", "n + 1 > 2",
		"length(s) > 1", "s LIKE 'x%'", "n BETWEEN m AND 2", "(n = 1) IS NULL", "n IN (1) AND n / 2 = 1",
		"m IN (1) OR n IN (2) AND s = n",
	} {
		if _, err := Partial(table(), expr); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", expr, err)
		}
	}
}

func TestNumberAndLike(t *testing.T) {
	for in, want := range map[string]string{"1": "1", "-.5": "-0.5", ".5": "0.5", "1.": "1.0", "2.25": "2.25"} {
		if got := number(in); got != want {
			t.Errorf("number(%s) = %s", in, got)
		}
	}
	if got := likeRegex(`a(b)%_\`); got != `^a\(b\).*.$` {
		t.Errorf("likeRegex: %s", got)
	}
}
