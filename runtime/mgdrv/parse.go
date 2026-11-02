package mgdrv

import (
	"encoding/json"
	"math"
	"strconv"
)

// parser compiles a command's JSON text into a template. A hand-written
// recursive descent rather than encoding/json, because BSON is ORDERED — a
// command's name must be its first key — and a Go map is not.

type parser struct {
	s    string
	i    int
	t    *template
	path []string
}

func compile(text string) (*template, error) {
	p := &parser{s: text, t: &template{}}
	p.ws()
	if !p.eat('{') {
		return nil, errTemplate
	}
	if err := p.members(); err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) || p.t.cmd == "" {
		return nil, errTemplate
	}
	return p.t, nil
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) eat(c byte) bool {
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *parser) raw(b []byte) {
	if n := len(p.t.ops); n > 0 && p.t.ops[n-1].kind == opRaw {
		p.t.ops[n-1].raw = append(p.t.ops[n-1].raw, b...)
		return
	}
	p.t.ops = append(p.t.ops, op{kind: opRaw, raw: b})
}

// members parses an object's members up to and including its '}'.
func (p *parser) members() error {
	if p.eat('}') {
		return nil
	}
	for {
		p.ws()
		key, err := p.str()
		if err != nil {
			return err
		}
		if len(p.path) == 0 && p.t.cmd == "" {
			p.t.cmd = key
		}
		if !p.eat(':') {
			return errTemplate
		}
		p.path = append(p.path, key)
		err = p.value(key)
		p.path = p.path[:len(p.path)-1]
		if err != nil {
			return err
		}
		if p.eat(',') {
			continue
		}
		if p.eat('}') {
			return nil
		}
		return errTemplate
	}
}

// value parses one value and emits it as the element name.
func (p *parser) value(name string) error {
	p.ws()
	if p.i >= len(p.s) {
		return errTemplate
	}
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		if n, ok := p.hole(); ok {
			p.t.ops = append(p.t.ops, op{kind: opArg, name: name, arg: n - 1})
			if n > p.t.nargs {
				p.t.nargs = n
			}
			return nil
		}
		p.noteCols()
		p.t.ops = append(p.t.ops, op{kind: opOpen, raw: appendName(nil, tDocument, name)})
		if err := p.members(); err != nil {
			return err
		}
		p.t.ops = append(p.t.ops, op{kind: opClose})
		return nil
	case c == '[':
		p.i++
		p.t.ops = append(p.t.ops, op{kind: opOpen, raw: appendName(nil, tArray, name)})
		if !p.eat(']') {
			for k := 0; ; k++ {
				p.path = append(p.path, argName(k))
				err := p.value(argName(k))
				p.path = p.path[:len(p.path)-1]
				if err != nil {
					return err
				}
				if p.eat(',') {
					continue
				}
				if p.eat(']') {
					break
				}
				return errTemplate
			}
		}
		p.t.ops = append(p.t.ops, op{kind: opClose})
		return nil
	case c == '"':
		s, err := p.str()
		if err != nil {
			return err
		}
		if len(p.path) == 3 && p.path[0] == "pipeline" && p.path[2] == "$count" {
			p.t.cols = []string{s}
		}
		p.raw(appendStr(nil, name, s))
		return nil
	case p.lit("true"):
		p.raw(appendBool(nil, name, true))
		return nil
	case p.lit("false"):
		p.raw(appendBool(nil, name, false))
		return nil
	case p.lit("null"):
		p.raw(appendName(nil, tNull, name))
		return nil
	}
	return p.number(name)
}

// noteCols records the result shape when the object about to be parsed is
// one that defines it. Called at the object's opening brace.
func (p *parser) noteCols() {
	d := len(p.path)
	switch {
	case d == 1 && (p.path[0] == "projection" || p.path[0] == "fields"):
	case d == 3 && p.path[0] == "pipeline" && p.path[2] == "$project":
	default:
		return
	}
	// Peek the keys without consuming: the members parse emits them.
	save := p.i
	var cols []string
	if !p.eat('}') {
		for {
			p.ws()
			k, err := p.str()
			if err != nil || !p.eat(':') || p.skip() != nil {
				break
			}
			cols = append(cols, k)
			if !p.eat(',') {
				break
			}
		}
	}
	p.i = save
	p.t.cols = cols
}

// skip passes over one value without emitting it.
func (p *parser) skip() error {
	p.ws()
	if p.i >= len(p.s) {
		return errTemplate
	}
	depth := 0
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '"':
			if _, err := p.str(); err != nil {
				return err
			}
			if depth == 0 {
				return nil
			}
			continue
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return nil // the enclosing container's close: not ours
			}
			depth--
			if depth == 0 {
				p.i++
				return nil
			}
		case ',':
			if depth == 0 {
				return nil
			}
		}
		p.i++
	}
	return nil
}

// hole recognises {"$arg":N} after its '{' and consumes it.
func (p *parser) hole() (int, bool) {
	save := p.i
	p.ws()
	if k, err := p.str(); err == nil && k == "$arg" && p.eat(':') {
		p.ws()
		j := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		n, err := strconv.Atoi(p.s[j:p.i])
		if err == nil && n >= 1 && p.eat('}') {
			return n, true
		}
	}
	p.i = save
	return 0, false
}

func (p *parser) lit(w string) bool {
	if len(p.s)-p.i >= len(w) && p.s[p.i:p.i+len(w)] == w {
		p.i += len(w)
		return true
	}
	return false
}

func (p *parser) str() (string, error) {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		return "", errTemplate
	}
	j := p.i + 1
	esc := false
	for ; j < len(p.s); j++ {
		if p.s[j] == '\\' {
			esc = true
			j++
			continue
		}
		if p.s[j] == '"' {
			break
		}
	}
	if j >= len(p.s) {
		return "", errTemplate
	}
	tok := p.s[p.i : j+1]
	p.i = j + 1
	if !esc {
		return tok[1 : len(tok)-1], nil
	}
	var s string
	if err := json.Unmarshal([]byte(tok), &s); err != nil {
		return "", errTemplate
	}
	return s, nil
}

func (p *parser) number(name string) error {
	j := p.i
	float := false
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '.' || c == 'e' || c == 'E' {
			float = true
		} else if !(c == '-' || c == '+' || (c >= '0' && c <= '9')) {
			break
		}
		p.i++
	}
	tok := p.s[j:p.i]
	if tok == "" {
		return errTemplate
	}
	if !float {
		if n, err := strconv.ParseInt(tok, 10, 64); err == nil {
			if n >= math.MinInt32 && n <= math.MaxInt32 {
				p.raw(appendInt32(nil, name, int32(n)))
			} else {
				p.raw(appendInt64(nil, name, n))
			}
			return nil
		}
	}
	f, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return errTemplate
	}
	b, _ := appendValue(nil, name, f)
	p.raw(b)
	return nil
}
