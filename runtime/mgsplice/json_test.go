package mgsplice

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func TestJSONContains(t *testing.T) {
	for in, want := range map[string]string{
		`{}`:         `[{"a":{"$type":"object","$not":{"$type":"array"}}}]`,
		`{"k":"v"}`:  `[{"a":{"$type":"object","$not":{"$type":"array"}}},{"a.k":{"$eq":"v","$not":{"$type":"array"}}}]`,
		`{"k":null}`: `[{"a":{"$type":"object","$not":{"$type":"array"}}},{"a.k":{"$type":"null"}}]`,
		`{"o":{"n":1.5}}`: `[{"a":{"$type":"object","$not":{"$type":"array"}}},` +
			`{"a.o":{"$type":"object","$not":{"$type":"array"}}},{"a.o.n":{"$eq":1.5,"$not":{"$type":"array"}}}]`,
		`[1,"x",true,false]`: `[{"a":{"$type":"array"}},{"a":{"$all":[1,"x",true,false]}}]`,
		`[{}]`:               `[{"a":{"$type":"array"}},{"a":{"$elemMatch":{"$type":"object"}}}]`,
		`[{"k":1}]`:          `[{"a":{"$type":"array"}},{"a":{"$elemMatch":{"$and":[{"k":{"$eq":1,"$not":{"$type":"array"}}}]}}}]`,
		`"s"`:                `[{"a":{"$eq":"s"}}]`,
	} {
		got, err := JSONContains("a", runtime.JSON(in))
		if err != nil || string(got) != want {
			t.Errorf("%s:\n got %s %v\nwant %s", in, got, err, want)
		}
		var v any
		if json.Unmarshal(got, &v) != nil {
			t.Errorf("%s: not JSON", got)
		}
	}
	for in, want := range map[string]error{
		`{"a.b":1}`:      ErrJSONKey,
		`{"$where":1}`:   ErrJSONKey,
		`{"":1}`:         ErrJSONKey,
		`[[1]]`:          ErrJSONPredicate,
		`[{"$x":1}]`:     ErrJSONKey,
		`{"o":{"$x":1}}`: ErrJSONKey,
		`[{"k":[[1]]}]`:  ErrJSONPredicate,
		`{`:              errNotJSON,
		`1 2`:            errNotJSON,
		``:               errNotJSON,
		`{"k" 1}`:        errNotJSON,
		`{1:2}`:          errNotJSON,
		`[1,]`:           errNotJSON,
		`{"k":[1,}`:      errNotJSON,
	} {
		if _, err := JSONContains("a", runtime.JSON(in)); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", in, err, want)
		}
	}
	deep := ""
	for range 102 {
		deep += "["
	}
	if _, err := JSONContains("a", runtime.JSON(deep)); err == nil {
		t.Error("unbounded nesting")
	}
}

func TestHasKeys(t *testing.T) {
	got, err := HasKeys("a", []string{"k"}, false)
	want := `[{"$or":[{"$expr":{"$eq":[{"$type":"$a"},"object"]},"a.k":{"$exists":true}},{"a":{"$eq":"k"}}]}]`
	if err != nil || string(got) != want {
		t.Errorf("got %s %v", got, err)
	}
	if got, _ := HasKeys("a", nil, true); string(got) != `[{"a":{"$ne":null}}]` {
		t.Error(string(got))
	}
	if got, _ := HasKeys("a", nil, false); string(got) != `[{"_id":{"$exists":false}}]` {
		t.Error(string(got))
	}
	if _, err := HasKeys("a", []string{"ok", "a.b"}, true); !errors.Is(err, ErrJSONKey) {
		t.Error("a dotted key", err)
	}
}
