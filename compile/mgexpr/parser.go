package mgexpr

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// parser reads one expression over t's columns, PostgreSQL's precedence:
// OR, AND, NOT, IS, comparison, BETWEEN/IN/LIKE, + -, *, unary minus, ::.
type parser struct {
	toks []token
	i    int
	t    *schema.Table
}

func parse(t *schema.Table, s string) (*node, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, t: t}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.i < len(p.toks) {
		return nil, fail("unexpected " + p.toks[p.i].text)
	}
	return n, nil
}

func (p *parser) peek(text string) bool {
	return p.i < len(p.toks) && p.toks[p.i].kind != 's' && p.toks[p.i].kind != 'q' && p.toks[p.i].text == text
}

func (p *parser) eat(text string) bool {
	if p.peek(text) {
		p.i++
		return true
	}
	return false
}

func (p *parser) want(text string) error {
	if !p.eat(text) {
		return fail("expected " + text)
	}
	return nil
}

func (p *parser) or() (*node, error) {
	return p.chain("or", '|', p.and)
}

func (p *parser) and() (*node, error) {
	return p.chain("and", '&', p.not)
}

func (p *parser) chain(word string, kind byte, next func() (*node, error)) (*node, error) {
	n, err := next()
	if err != nil {
		return nil, err
	}
	kids := []*node{n}
	for p.eat(word) {
		m, err := next()
		if err != nil {
			return nil, err
		}
		kids = append(kids, m)
	}
	if len(kids) == 1 {
		return n, nil
	}
	for _, k := range kids {
		if err := boolean(k); err != nil {
			return nil, err
		}
	}
	return &node{kind: kind, kids: kids, cls: "bool"}, nil
}

func boolean(n *node) error {
	if n.cls != "bool" && n.cls != "null" {
		return fail(kind(n.cls) + " where a condition belongs")
	}
	return nil
}

func (p *parser) not() (*node, error) {
	if p.eat("not") {
		n, err := p.not()
		if err != nil {
			return nil, err
		}
		if err := boolean(n); err != nil {
			return nil, err
		}
		return &node{kind: '!', kids: []*node{n}, cls: "bool"}, nil
	}
	return p.is()
}

func (p *parser) is() (*node, error) {
	n, err := p.compare()
	if err != nil {
		return nil, err
	}
	for p.eat("is") {
		neg := p.eat("not")
		switch {
		case p.eat("null"):
			n = &node{kind: 'z', neg: neg, kids: []*node{n}, cls: "bool"}
		default:
			return nil, fail("IS other than IS [NOT] NULL")
		}
	}
	return n, nil
}

var compares = []string{"=", "<>", "!=", "<=", ">=", "<", ">"}

func (p *parser) compare() (*node, error) {
	n, err := p.special()
	if err != nil {
		return nil, err
	}
	for _, op := range compares {
		if p.eat(op) {
			m, err := p.special()
			if err != nil {
				return nil, err
			}
			if op == "!=" {
				op = "<>"
			}
			return cmp(op, n, m)
		}
	}
	return n, nil
}

// cmp is a comparison, refused where MongoDB's answer is not SQL's.
func cmp(op string, a, b *node) (*node, error) {
	cls, err := unify(a, b)
	if err != nil {
		return nil, err
	}
	if op != "=" && op != "<>" {
		switch {
		case cls == "text" || cls == "str":
			return nil, fail("ordering text, which PostgreSQL compares by collation and MongoDB by bytes")
		case strings.HasPrefix(cls, "enum "):
			return nil, fail("ordering an enum, which PostgreSQL orders by declaration")
		case cls == "bool" || cls == "uuid" || cls == "bytea":
			return nil, fail("ordering " + kind(cls))
		}
	}
	return &node{kind: 'm', op: op, kids: []*node{a, b}, cls: "bool"}, nil
}

// unify is the kind two compared values share, or a refusal.
func unify(a, b *node) (string, error) {
	switch {
	case a.cls == "null":
		return b.cls, nil
	case b.cls == "null":
		return a.cls, nil
	case numeric(a.cls) && numeric(b.cls):
		n := a.nums | b.nums
		if n&numDecimal != 0 && n&(numFrac|numFloat) != 0 {
			return "", fail("a numeric column against a fractional literal or a float — MongoDB compares " +
				"a decimal and a double exactly, PostgreSQL after a cast")
		}
		return "int", nil
	case a.cls == b.cls:
		return a.cls, nil
	case a.cls == "str" && (b.cls == "text" || strings.HasPrefix(b.cls, "enum ")):
		return b.cls, nil
	case b.cls == "str" && (a.cls == "text" || strings.HasPrefix(a.cls, "enum ")):
		return a.cls, nil
	}
	return "", fail("comparing " + a.cls + " with " + b.cls + ", which SQL would cast and MongoDB orders by BSON type")
}

func numeric(cls string) bool {
	return cls == "int" || cls == "num" || cls == "float" || cls == "decimal"
}

// special is BETWEEN, IN and LIKE, desugared where SQL defines them by the
// simpler forms: BETWEEN is two comparisons, IN a disjunction of equalities.
func (p *parser) special() (*node, error) {
	n, err := p.sum()
	if err != nil {
		return nil, err
	}
	at := p.i
	neg := p.eat("not")
	var out *node
	switch {
	case p.eat("between"):
		lo, err := p.sum()
		if err != nil {
			return nil, err
		}
		if err := p.want("and"); err != nil {
			return nil, err
		}
		hi, err := p.sum()
		if err != nil {
			return nil, err
		}
		a, err := cmp(">=", n, lo)
		if err != nil {
			return nil, err
		}
		b, err := cmp("<=", n, hi)
		if err != nil {
			return nil, err
		}
		out = &node{kind: '&', kids: []*node{a, b}, cls: "bool"}
	case p.eat("in"):
		if err := p.want("("); err != nil {
			return nil, err
		}
		var eqs []*node
		for {
			v, err := p.sum()
			if err != nil {
				return nil, err
			}
			e, err := cmp("=", n, v)
			if err != nil {
				return nil, err
			}
			eqs = append(eqs, e)
			if !p.eat(",") {
				break
			}
		}
		if err := p.want(")"); err != nil {
			return nil, err
		}
		out = &node{kind: '|', kids: eqs, cls: "bool", op: "in"}
	case p.eat("like"):
		if p.i >= len(p.toks) || p.toks[p.i].kind != 's' {
			return nil, fail("LIKE with a pattern that is not a string literal")
		}
		pat := p.toks[p.i].text
		p.i++
		if n.cls != "text" {
			return nil, fail("LIKE on " + kind(n.cls))
		}
		return &node{kind: 'k', neg: neg, str: pat, kids: []*node{n}, cls: "bool"}, nil
	default:
		p.i = at
		return n, nil
	}
	if neg {
		out = &node{kind: '!', kids: []*node{out}, cls: "bool"}
	}
	return out, nil
}

func (p *parser) sum() (*node, error) {
	return p.arith([]string{"+", "-"}, p.product)
}

func (p *parser) product() (*node, error) {
	return p.arith([]string{"*"}, p.unary)
}

func (p *parser) arith(ops []string, next func() (*node, error)) (*node, error) {
	n, err := next()
	if err != nil {
		return nil, err
	}
	for {
		op := ""
		for _, o := range ops {
			if p.eat(o) {
				op = o
			}
		}
		if op == "" {
			if p.peek("/") || p.peek("%") {
				return nil, fail("division, which truncates in SQL and not in MongoDB")
			}
			return n, nil
		}
		m, err := next()
		if err != nil {
			return nil, err
		}
		if n, err = arithmetic(op, n, m); err != nil {
			return nil, err
		}
	}
}

func arithmetic(op string, a, b *node) (*node, error) {
	for _, x := range []*node{a, b} {
		if !numeric(x.cls) && x.cls != "null" {
			return nil, fail("arithmetic on " + kind(x.cls))
		}
	}
	if _, err := unify(a, b); err != nil {
		return nil, err
	}
	return &node{kind: 'a', op: op, kids: []*node{a, b}, cls: "int", nums: a.nums | b.nums}, nil
}

func (p *parser) unary() (*node, error) {
	if p.eat("-") {
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		if n.kind == 'l' && numeric(n.cls) {
			return negate(n), nil
		}
		return arithmetic("*", &node{kind: 'l', lit: "-1", cls: "int", nums: numInt}, n)
	}
	n, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.eat("::") {
		if n, err = p.cast(n); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func negate(n *node) *node {
	m := *n
	m.str = "-" + n.str
	m.lit = number(m.str)
	return &m
}

// cast accepts a literal cast to its own kind of type — which changes nothing
// a document can see — and nothing else.
func (p *parser) cast(n *node) (*node, error) {
	if p.i >= len(p.toks) {
		return nil, fail("a cast with no type")
	}
	typ := p.toks[p.i].text
	p.i++
	if typ == "double" {
		p.eat("precision")
	}
	if typ == "character" {
		p.eat("varying")
	}
	if n.kind != 'l' {
		return nil, fail("a cast of a non-literal (::" + typ + ")")
	}
	switch typ {
	case "int", "integer", "int2", "int4", "int8", "smallint", "bigint":
		if n.cls == "int" {
			return n, nil
		}
	case "numeric", "decimal", "real", "float4", "float8", "double":
		if numeric(n.cls) {
			return n, nil
		}
	case "text", "varchar", "character":
		if n.cls == "str" {
			m := *n
			m.cls = "text"
			return &m, nil
		}
	case "bool", "boolean":
		if n.cls == "bool" {
			return n, nil
		}
	}
	return nil, fail("the cast ::" + typ)
}

func (p *parser) primary() (*node, error) {
	if p.i >= len(p.toks) {
		return nil, fail("an expression that ends early")
	}
	tk := p.toks[p.i]
	p.i++
	switch tk.kind {
	case 'n':
		cls, nums := "int", numInt
		if strings.Contains(tk.text, ".") {
			cls, nums = "num", numFrac
		}
		return &node{kind: 'l', lit: number(tk.text), str: tk.text, cls: cls, nums: nums}, nil
	case 's':
		return &node{kind: 'l', lit: `{"$literal":` + mongo.Str(tk.text) + `}`, str: tk.text, cls: "str"}, nil
	case 'q':
		return p.column(tk.text)
	case 'o':
		if tk.text == "(" {
			n, err := p.or()
			if err != nil {
				return nil, err
			}
			return n, p.want(")")
		}
		return nil, fail("unexpected " + tk.text)
	}
	switch tk.text {
	case "true", "false":
		return &node{kind: 'l', lit: tk.text, cls: "bool"}, nil
	case "null":
		return &node{kind: 'l', lit: "null", cls: "null"}, nil
	}
	if p.eat("(") {
		return p.call(tk.text)
	}
	return p.column(tk.text)
}

// number is a numeric literal's JSON: an int32 as digits, a wider integer as
// $numberLong (mgdrv reads it), a fraction as a double.
func number(s string) string {
	if strings.Contains(s, ".") {
		if s[0] == '.' {
			return "0" + s
		}
		if strings.HasPrefix(s, "-.") {
			return "-0" + s[1:]
		}
		if strings.HasSuffix(s, ".") {
			return s + "0"
		}
		return s
	}
	if n, err := strconv.ParseInt(s, 10, 32); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return `{"$numberLong":` + mongo.Str(s) + `}`
}

func (p *parser) column(name string) (*node, error) {
	c := p.t.Column(name)
	if c == nil {
		return nil, fail(name + ", which is not a column of " + p.t.Name)
	}
	if c.Type.Array || mongo.IsJSON(c) {
		return nil, fail("the array or json column " + name)
	}
	n := &node{kind: 'c', col: c}
	switch {
	case c.Type.Enum:
		n.cls = "enum " + c.Type.Name
	default:
		switch c.Type.Name {
		case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
			n.cls, n.nums = "int", numInt
		case schema.TypeFloat4, schema.TypeFloat8:
			n.cls, n.nums = "float", numFloat
		case schema.TypeNumeric:
			n.cls, n.nums = "decimal", numDecimal
		case schema.TypeText, schema.TypeVarchar:
			n.cls = "text"
		case schema.TypeBool:
			n.cls = "bool"
		case schema.TypeUUID, schema.TypeTimestamptz, schema.TypeDate, schema.TypeBytea:
			n.cls = c.Type.Name
		default:
			return nil, fail("the " + c.Type.SQL() + " column " + name)
		}
	}
	return n, nil
}

func (p *parser) call(name string) (*node, error) {
	var args []*node
	if !p.eat(")") {
		for {
			a, err := p.or()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if !p.eat(",") {
				break
			}
		}
		if err := p.want(")"); err != nil {
			return nil, err
		}
	}
	n := &node{kind: 'f', op: name, kids: args}
	switch name {
	case "length", "char_length", "character_length":
		if len(args) != 1 || args[0].cls != "text" && args[0].cls != "str" {
			return nil, fail(name + " of anything but one text value")
		}
		n.op, n.cls, n.nums = "length", "int", numInt
	case "abs":
		if len(args) != 1 || !numeric(args[0].cls) {
			return nil, fail("abs of anything but one number")
		}
		n.cls, n.nums = args[0].cls, args[0].nums
	case "coalesce":
		if len(args) < 2 {
			return nil, fail("coalesce of fewer than two values")
		}
		n.cls, n.nums = args[0].cls, args[0].nums
		for _, a := range args[1:] {
			cls, err := unify(&node{cls: n.cls, nums: n.nums}, a)
			if err != nil {
				return nil, err
			}
			n.cls, n.nums = cls, n.nums|a.nums
		}
	default:
		return nil, fail("the function " + name + "()")
	}
	return n, nil
}

// kind is a value's kind as a refusal says it: "an int value".
func kind(cls string) string {
	if strings.IndexByte("aeiou", cls[0]) >= 0 {
		return "an " + cls + " value"
	}
	return "a " + cls + " value"
}
