package mgsplice

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// The json predicates bind a filter, not a value: MongoDB has no containment
// operator, so `doc @> v` is the conjunction of what v says about each path.
// The filters are built here, in Go, from the caller's document, and bound as
// ONE value — so a statement's text never depends on the document's shape,
// and nothing the caller wrote is ever read as an operator: their keys become
// field paths only after a check, and their values are compared with $eq.
//
// The translation keeps PostgreSQL's answers where MongoDB's element-wise
// matching would differ. A scalar member must not match an array that merely
// contains it ({"a":["x"]} does not contain {"a":"x"}), and an object member
// must be an object, not an array of them — so each is stated with a type
// test beside it.

// ErrJSONKey is a json predicate's key that cannot be a field path: empty,
// starting with $, or containing a dot or a NUL.
var ErrJSONKey = errors.New("storm: a json key used in a MongoDB predicate cannot be empty, start with $ or contain a dot")

// ErrJSONPredicate is a containment document this back end cannot translate
// faithfully — an array nested directly in an array.
var ErrJSONPredicate = errors.New("storm: a json containment this back end cannot translate faithfully (an array directly inside an array)")

// JSONContains is `field @> v` as a list of filters, ANDed by the fragment
// that binds it.
func JSONContains(field string, v runtime.JSON) (runtime.JSON, error) {
	n, err := parse(v)
	if err != nil {
		return nil, err
	}
	var out []string
	switch n.kind {
	case kObject, kArray:
		if out, err = contain(field, n, out); err != nil {
			return nil, err
		}
	default:
		// A scalar is contained by an equal scalar or by an array holding it,
		// which is exactly MongoDB's element-wise equality.
		out = append(out, `{`+quote(field)+`:{"$eq":`+n.text+`}}`)
	}
	return list(out), nil
}

// HasKeys is `field ?| keys` (all false) or `field ?& keys` (all true), as a
// list the fragment ORs or ANDs. A key is present when the column is an
// object with that member, or an array or string equal to or holding it —
// the three readings PostgreSQL gives `?`.
func HasKeys(field string, keys []string, all bool) (runtime.JSON, error) {
	if len(keys) == 0 {
		if all {
			// Every document has all of no keys; a NULL column has none.
			return list([]string{`{` + quote(field) + `:{"$ne":null}}`}), nil
		}
		return list([]string{`{"_id":{"$exists":false}}`}), nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !validKey(k) {
			return nil, ErrJSONKey
		}
		out = append(out, `{"$or":[{"$expr":{"$eq":[{"$type":`+quote("$"+field)+`},"object"]},`+
			quote(field+"."+k)+`:{"$exists":true}},{`+quote(field)+`:{"$eq":`+quote(k)+`}}]}`)
	}
	return list(out), nil
}

// contain appends the filters that say the value at path contains n.
func contain(path string, n node, out []string) ([]string, error) {
	p := quote(path)
	switch n.kind {
	case kObject:
		out = append(out, `{`+p+`:{"$type":"object","$not":{"$type":"array"}}}`)
		for _, m := range n.obj {
			if !validKey(m.key) {
				return nil, ErrJSONKey
			}
			var err error
			if out, err = member(path+"."+m.key, m, out); err != nil {
				return nil, err
			}
		}
	case kArray:
		out = append(out, `{`+p+`:{"$type":"array"}}`)
		var scalars []string
		for _, e := range n.arr {
			switch e.kind {
			case kObject:
				cond, err := elem(e)
				if err != nil {
					return nil, err
				}
				out = append(out, `{`+p+`:{"$elemMatch":`+cond+`}}`)
			case kArray:
				return nil, ErrJSONPredicate
			default:
				scalars = append(scalars, e.text)
			}
		}
		if len(scalars) > 0 {
			out = append(out, `{`+p+`:{"$all":[`+strings.Join(scalars, ",")+`]}}`)
		}
	}
	return out, nil
}

// member is one object member at path.
func member(path string, n node, out []string) ([]string, error) {
	switch n.kind {
	case kObject, kArray:
		return contain(path, n, out)
	case kNull:
		return append(out, `{`+quote(path)+`:{"$type":"null"}}`), nil
	}
	return append(out, `{`+quote(path)+`:{"$eq":`+n.text+`,"$not":{"$type":"array"}}}`), nil
}

// elem is an $elemMatch condition: the element is an object containing n.
func elem(n node) (string, error) {
	if len(n.obj) == 0 {
		return `{"$type":"object"}`, nil
	}
	var conds []string
	for _, m := range n.obj {
		if !validKey(m.key) {
			return "", ErrJSONKey
		}
		var err error
		if conds, err = member(m.key, m, conds); err != nil {
			return "", err
		}
	}
	return `{"$and":` + string(list(conds)) + `}`, nil
}

func validKey(k string) bool {
	return k != "" && k[0] != '$' && !strings.ContainsAny(k, ".\x00")
}

func list(conds []string) runtime.JSON {
	return runtime.JSON("[" + strings.Join(conds, ",") + "]")
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A parsed JSON value that keeps object members in order: an object's
// members are its obj, each carrying its key.
type node struct {
	kind uint8
	key  string // the member's key, inside an object
	text string // a scalar's JSON text
	obj  []node
	arr  []node
}

const (
	kScalar uint8 = iota
	kNull
	kObject
	kArray
)

var errNotJSON = errors.New("storm: a json predicate's value is not one JSON document")

func parse(v runtime.JSON) (node, error) {
	d := json.NewDecoder(strings.NewReader(string(v)))
	d.UseNumber()
	n, err := parseNode(d, 0)
	if err != nil {
		return node{}, err
	}
	if _, err := d.Token(); err != io.EOF {
		return node{}, errNotJSON
	}
	return n, nil
}

func parseNode(d *json.Decoder, depth int) (node, error) {
	if depth > 100 {
		return node{}, errNotJSON
	}
	tok, err := d.Token()
	if err != nil {
		return node{}, errNotJSON
	}
	switch v := tok.(type) {
	case json.Delim:
		var n node
		if v == '{' {
			n.kind = kObject
			for d.More() {
				k, err := d.Token()
				ks, ok := k.(string)
				if err != nil || !ok {
					return node{}, errNotJSON
				}
				c, err := parseNode(d, depth+1)
				if err != nil {
					return node{}, err
				}
				c.key = ks
				n.obj = append(n.obj, c)
			}
		} else {
			n.kind = kArray
			for d.More() {
				c, err := parseNode(d, depth+1)
				if err != nil {
					return node{}, err
				}
				n.arr = append(n.arr, c)
			}
		}
		if _, err := d.Token(); err != nil {
			return node{}, errNotJSON
		}
		return n, nil
	case string:
		return node{text: quote(v)}, nil
	case json.Number:
		return node{text: string(v)}, nil
	case bool:
		if v {
			return node{text: "true"}, nil
		}
		return node{text: "false"}, nil
	}
	return node{kind: kNull, text: "null"}, nil
}
