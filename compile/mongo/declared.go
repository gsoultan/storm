package mongo

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// The declared cross-table reads — joins and unions — lowered to one
// aggregation each, for the subset that gives SQL's answer. Every function
// here returns an error naming the construct it cannot lower; compile/mgddl
// reports it at generate time, and codegen renders what comes back, so the
// check and the statement cannot disagree.

// Resolve maps a column reference, qualified by the alias it was declared
// under ("" for the reading table), to its path in the documents the stage
// sees and the column itself.
type Resolve func(tbl, col string) (path string, c *schema.Column, err error)

// family is the class of values a type compares within: MongoDB orders
// across numeric types by value as SQL does, but compares nothing else across
// a type boundary SQL would cast over.
func family(t schema.Type) string {
	if t.Array {
		return "array"
	}
	if t.Enum {
		return "enum:" + t.Name
	}
	switch t.Name {
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8, schema.TypeNumeric, schema.TypeFloat4, schema.TypeFloat8:
		return "number"
	case schema.TypeText, schema.TypeVarchar:
		return "text"
	case schema.TypeTimestamptz, schema.TypeTimestamp, schema.TypeDate:
		return "time"
	}
	return t.Name
}

// Filter renders a declared predicate as a query filter matching exactly the
// rows SQL's predicate is TRUE for. A comparison never matches a null or
// missing field, which is SQL's UNKNOWN; without NOT, UNKNOWN and FALSE
// select the same rows under AND and OR, so the query language's answer is
// SQL's. NOT is refused for that reason: $nor matches a null field where
// SQL's NOT over UNKNOWN is UNKNOWN.
func Filter(c schema.Cond, resolve Resolve, param func(i int) string) (string, error) {
	switch c.Kind {
	case schema.CondAnd, schema.CondOr:
		op := `{"$and":[`
		if c.Kind == schema.CondOr {
			op = `{"$or":[`
		}
		parts := make([]string, len(c.Args))
		for i, a := range c.Args {
			f, err := Filter(a, resolve, param)
			if err != nil {
				return "", err
			}
			parts[i] = f
		}
		return op + strings.Join(parts, ",") + `]}`, nil
	case schema.CondIsNull, schema.CondIsNotNull:
		if c.Left.Kind != schema.ExprCol {
			return "", errors.New("IS NULL over an expression")
		}
		path, _, err := resolve(c.Left.Tbl, c.Left.Col)
		if err != nil {
			return "", err
		}
		if c.Kind == schema.CondIsNull {
			return `{` + Str(path) + `:null}`, nil
		}
		return `{` + Str(path) + `:{"$ne":null}}`, nil
	case schema.CondCmp:
		return cmpFilter(c, resolve, param)
	}
	return "", errors.New("NOT, which MongoDB's $nor answers for a NULL where SQL's NOT is UNKNOWN")
}

var flipped = map[schema.CmpOp]schema.CmpOp{
	schema.OpEq: schema.OpEq, schema.OpNe: schema.OpNe,
	schema.OpLt: schema.OpGt, schema.OpGt: schema.OpLt,
	schema.OpLte: schema.OpGte, schema.OpGte: schema.OpLte,
}

var cmpOps = map[schema.CmpOp]string{
	schema.OpEq: "$eq", schema.OpLt: "$lt", schema.OpLte: "$lte", schema.OpGt: "$gt", schema.OpGte: "$gte",
}

func cmpFilter(c schema.Cond, resolve Resolve, param func(i int) string) (string, error) {
	l, r, op := c.Left, c.Right, c.Op
	if l.Kind != schema.ExprCol {
		l, r, op = r, l, flipped[op]
	}
	if l.Kind != schema.ExprCol || r.Kind == schema.ExprCol {
		return "", errors.New("a comparison other than of a column with a value, which a query filter cannot state")
	}
	path, col, err := resolve(l.Tbl, l.Col)
	if err != nil {
		return "", err
	}
	if col.Type.Array || IsJSON(col) {
		return "", errors.New("a comparison of an array or json column, which MongoDB matches element-wise")
	}
	var v string
	switch r.Kind {
	case schema.ExprParam:
		if param == nil {
			return "", errors.New("a parameter, which only a union declares")
		}
		if family(r.Type) != family(col.Type) {
			return "", errors.New("a comparison of " + col.Name + " with a parameter of another type")
		}
		v = param(r.Param)
	case schema.ExprLit:
		if v, err = literal(r.Lit, col); err != nil {
			return "", err
		}
	default:
		return "", errors.New("a comparison with an expression, which a query filter cannot state")
	}
	if op != schema.OpEq && op != schema.OpNe {
		switch f := family(col.Type); {
		case f == "text" || strings.HasPrefix(f, "enum:"):
			return "", errors.New("ordering " + col.Name + ", which PostgreSQL orders by collation or declaration and MongoDB by bytes")
		case f == "bool" || f == schema.TypeUUID || f == schema.TypeBytea:
			return "", errors.New("ordering " + col.Name + ", a " + col.Type.SQL())
		}
	}
	if op == schema.OpNe {
		// <> is UNKNOWN for a NULL; $ne would match it.
		return `{` + Str(path) + `:{"$nin":[` + v + `,null]}}`, nil
	}
	return `{` + Str(path) + `:{` + Str(cmpOps[op]) + `:` + v + `}}`, nil
}

// literal renders a declared literal compared with col, refusing a pair SQL
// would cast across or MongoDB would compare differently.
func literal(l schema.Literal, col *schema.Column) (string, error) {
	want := family(col.Type)
	lt := schema.Type{Name: l.Kind}
	if want != family(lt) && !(strings.HasPrefix(want, "enum:") && l.Kind == schema.TypeText) {
		return "", errors.New("a comparison of " + col.Name + " (" + col.Type.SQL() + ") with a " + l.Kind + " literal")
	}
	switch l.Kind {
	case schema.TypeText:
		return Str(l.S), nil
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
		return strconv.FormatInt(l.I, 10), nil
	case schema.TypeFloat8:
		if col.Type.Name == schema.TypeNumeric {
			return "", errors.New("a comparison of a decimal with a float literal, which SQL casts and MongoDB does not")
		}
		if math.IsNaN(l.F) || math.IsInf(l.F, 0) {
			return "", errors.New("a NaN or infinite literal")
		}
		return strconv.FormatFloat(l.F, 'g', -1, 64), nil
	case schema.TypeBool:
		return strconv.FormatBool(l.B), nil
	}
	return "", errors.New("a " + l.Kind + " literal in a filter, which the command text cannot carry; use a parameter")
}

// ProjectLiteral renders a declared literal as an aggregation expression, for
// a union branch's constant column.
func ProjectLiteral(l schema.Literal) (string, error) {
	switch l.Kind {
	case schema.TypeText:
		return `{"$literal":` + Str(l.S) + `}`, nil
	case schema.TypeInt2, schema.TypeInt4:
		return `{"$toInt":` + strconv.FormatInt(l.I, 10) + `}`, nil
	case schema.TypeInt8:
		return `{"$toLong":` + strconv.FormatInt(l.I, 10) + `}`, nil
	case schema.TypeFloat8:
		if math.IsNaN(l.F) || math.IsInf(l.F, 0) {
			return "", errors.New("a NaN or infinite literal")
		}
		return `{"$toDouble":` + strconv.FormatFloat(l.F, 'g', -1, 64) + `}`, nil
	case schema.TypeBool:
		return strconv.FormatBool(l.B), nil
	case schema.TypeNumeric:
		return `{"$toDecimal":` + Str(l.S) + `}`, nil
	case schema.TypeTimestamptz:
		return `{"$toDate":` + Str(l.S) + `}`, nil
	}
	return "", errors.New("a " + l.Kind + " literal")
}

// orderStages sorts by each term behind a null flag, so a NULL sorts where
// SQL puts it: last ascending and first descending unless nullsFirst says
// otherwise. paths are what each term reads; the flags are named _nullN.
func orderStages(paths []string, desc []bool, nullsFirst []*bool) string {
	if len(paths) == 0 {
		return ""
	}
	var flags, sort []string
	for i, p := range paths {
		dir, fdir := "1", "1"
		if desc[i] {
			dir = "-1"
		}
		first := desc[i]
		if nullsFirst != nil && nullsFirst[i] != nil {
			first = *nullsFirst[i]
		}
		if first {
			fdir = "-1"
		}
		flag := "_null" + strconv.Itoa(i)
		flags = append(flags, Str(flag)+`:{"$cond":[{"$eq":[{"$ifNull":["$`+p+`",null]},null]},1,0]}`)
		sort = append(sort, Str(flag)+":"+fdir, Str(p)+":"+dir)
	}
	return `,{"$addFields":{` + strings.Join(flags, ",") + `}},{"$sort":{` + strings.Join(sort, ",") + `}}`
}

// Join lowers a declared join on t to the stages after the call-site
// $match: a $lookup and $unwind per joined table, the declared filter, the
// order, the page (holes 2 and 1 — the skip and the cap, as a find's), and
// the projection. Equality joins only: each ON is columns of the joined
// table equal to columns already in scope, which $lookup answers with SQL's
// NULL — a NULL key joins nothing.
func Join(s *schema.Schema, t *schema.Table, j *schema.Join) (string, error) {
	if len(j.CTEs) > 0 {
		return "", errors.New("a CTE, which this back end does not generate")
	}
	aliases := map[string]string{} // alias → path prefix
	tables := map[string]*schema.Table{}
	resolve := func(tbl, col string) (string, *schema.Column, error) {
		if pre, ok := aliases[tbl]; ok {
			tt := tables[tbl]
			c := tt.Column(col)
			if c == nil {
				return "", nil, errors.New("no column " + tbl + "." + col)
			}
			return pre + Field(tt, col), c, nil
		}
		if tbl != "" && tbl != t.Name {
			return "", nil, errors.New("the alias " + tbl + ", not yet joined")
		}
		c := t.Column(col)
		if c == nil {
			return "", nil, errors.New("no column " + t.Name + "." + col)
		}
		return Field(t, col), c, nil
	}
	var b strings.Builder
	for i, jt := range j.Tables {
		if jt.Kind == schema.JoinElements {
			return "", errors.New("a join over json array elements")
		}
		target := s.Table(jt.Table)
		switch {
		case target == nil || jt.Table == "":
			return "", errors.New("a join to something other than a table")
		case EmbedOf(target) != nil:
			return "", errors.New("a join to " + target.Name + ", which is embedded and has no collection")
		case target.Generated:
			return "", errors.New("a join to a generated join table")
		}
		if _, dup := aliases[jt.Alias]; dup || jt.Alias == t.Name || jt.Alias == "" {
			return "", errors.New("the alias " + jt.Alias + " twice")
		}
		var pairs [][2]schema.Expr
		var walk func(c schema.Cond) bool
		walk = func(c schema.Cond) bool {
			if c.Kind == schema.CondAnd {
				for _, a := range c.Args {
					if !walk(a) {
						return false
					}
				}
				return true
			}
			if c.Kind != schema.CondCmp || c.Op != schema.OpEq || c.Left.Kind != schema.ExprCol || c.Right.Kind != schema.ExprCol {
				return false
			}
			l, r := c.Left, c.Right
			if r.Tbl == jt.Alias {
				l, r = r, l
			}
			if l.Tbl != jt.Alias || r.Tbl == jt.Alias {
				return false
			}
			pairs = append(pairs, [2]schema.Expr{l, r})
			return true
		}
		if !walk(jt.On) || len(pairs) == 0 {
			return "", errors.New("a join to " + jt.Alias + " on other than equal columns, which $lookup cannot answer")
		}
		var lets, eqs []string
		for k, p := range pairs {
			fc := target.Column(p[0].Col)
			if fc == nil {
				return "", errors.New("no column " + jt.Alias + "." + p[0].Col)
			}
			lp, lc, err := resolve(p[1].Tbl, p[1].Col)
			if err != nil {
				return "", err
			}
			if family(fc.Type) != family(lc.Type) || fc.Type.Array || lc.Type.Array || IsJSON(fc) || IsJSON(lc) {
				return "", errors.New("a join of " + lc.Name + " with " + fc.Name + ", which SQL would cast or MongoDB compares element-wise")
			}
			v := "v" + strconv.Itoa(k)
			lets = append(lets, Str(v)+`:{"$ifNull":["$`+lp+`",null]}`)
			eqs = append(eqs, `{"$ne":["$$`+v+`",null]}`, `{"$eq":["$`+Field(target, fc.Name)+`","$$`+v+`"]}`)
		}
		as := "_j" + strconv.Itoa(i)
		b.WriteString(`,{"$lookup":{"from":` + Str(target.Name) + `,"let":{` + strings.Join(lets, ",") +
			`},"pipeline":[{"$match":{"$expr":{"$and":[` + strings.Join(eqs, ",") + `]}}}`)
		if target.SoftDeletes() {
			b.WriteString(`,{"$match":{` + Str(Field(target, target.SoftDelete)) + `:null}}`)
		}
		b.WriteString(`],"as":` + Str(as) + `}},{"$unwind":{"path":"$` + as + `","preserveNullAndEmptyArrays":` +
			strconv.FormatBool(jt.Kind == schema.JoinLeft) + `}}`)
		aliases[jt.Alias] = as + "."
		tables[jt.Alias] = target
	}
	if j.Where != nil {
		f, err := Filter(*j.Where, resolve, nil)
		if err != nil {
			return "", err
		}
		b.WriteString(`,{"$match":` + f + `}`)
	}
	var paths []string
	var desc []bool
	for _, o := range j.OrderBy {
		if o.Expr.Kind != schema.ExprCol {
			return "", errors.New("an order by an expression")
		}
		p, c, err := resolve(o.Expr.Tbl, o.Expr.Col)
		if err != nil {
			return "", err
		}
		if c.Type.Array || IsJSON(c) {
			return "", errors.New("an order by an array or json column")
		}
		paths = append(paths, p)
		desc = append(desc, o.Desc)
	}
	b.WriteString(orderStages(paths, desc, nil))
	b.WriteString(`,{"$skip":` + Arg(2) + `},{"$limit":` + Arg(1) + `}`)
	var proj []string
	for _, c := range j.Select {
		if c.Expr.Kind != schema.ExprCol {
			return "", errors.New("the output " + c.As + ", an expression, which this back end does not generate")
		}
		p, _, err := resolve(c.Expr.Tbl, c.Expr.Col)
		if err != nil {
			return "", err
		}
		proj = append(proj, Str(c.As)+`:"$`+p+`"`)
	}
	b.WriteString(`,{"$project":{` + strings.Join(proj, ",") + `}}]}`)
	return b.String(), nil
}

// Union lowers a declared union to one aggregation: the first branch's
// collection, each further branch a $unionWith, every branch projected to
// the shared columns (a missing field as null, so a row is the same shape
// whichever branch it came from), DISTINCT a $group over all of them, then
// the order and the cap. Parameters are holes 1..k and the cap is k+1.
func Union(s *schema.Schema, u *schema.Union) (string, error) {
	param := func(i int) string { return Arg(i + 1) }
	branch := func(br schema.UnionBranch) (string, string, error) {
		t := s.Table(br.Table)
		switch {
		case t == nil:
			return "", "", errors.New("a branch over " + br.Table + ", which is not a table")
		case EmbedOf(t) != nil:
			return "", "", errors.New("a branch over " + t.Name + ", which is embedded and has no collection")
		}
		resolve := func(tbl, col string) (string, *schema.Column, error) {
			c := t.Column(col)
			if c == nil || (tbl != "" && tbl != t.Name) {
				return "", nil, errors.New("no column " + t.Name + "." + col)
			}
			return Field(t, col), c, nil
		}
		var match []string
		if t.SoftDeletes() {
			match = append(match, `{`+Str(Field(t, t.SoftDelete))+`:null}`)
		}
		if br.Where != nil {
			f, err := Filter(*br.Where, resolve, param)
			if err != nil {
				return "", "", err
			}
			match = append(match, f)
		}
		m := "{}"
		switch len(match) {
		case 1:
			m = match[0]
		case 2:
			m = `{"$and":[` + match[0] + `,` + match[1] + `]}`
		}
		if len(br.Exprs) != len(u.Cols) {
			return "", "", errors.New("a branch over " + t.Name + " of another width")
		}
		var proj []string
		for i, e := range br.Exprs {
			var v string
			switch e.Kind {
			case schema.ExprCol:
				p, c, err := resolve(e.Tbl, e.Col)
				if err != nil {
					return "", "", err
				}
				if !widens(c.Type, u.Cols[i].Type) {
					return "", "", errors.New("the column " + u.Cols[i].As + " from " + t.Name + " of another type")
				}
				v = `{"$ifNull":["$` + p + `",null]}`
			case schema.ExprLit:
				var err error
				if v, err = ProjectLiteral(e.Lit); err != nil {
					return "", "", errors.New("the column " + u.Cols[i].As + " from " + t.Name + ": " + err.Error())
				}
			default:
				return "", "", errors.New("the column " + u.Cols[i].As + " from " + t.Name + ", an expression, which this back end does not generate")
			}
			proj = append(proj, Str(u.Cols[i].As)+`:`+v)
		}
		return `{"$match":` + m + `},{"$project":{` + strings.Join(proj, ",") + `}}`, t.Name, nil
	}
	if len(u.Branches) == 0 {
		return "", errors.New("a union of no branches")
	}
	first, coll, err := branch(u.Branches[0])
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`{"aggregate":` + Str(coll) + `,"cursor":{},"pipeline":[` + first)
	for _, br := range u.Branches[1:] {
		stages, coll, err := branch(br)
		if err != nil {
			return "", err
		}
		b.WriteString(`,{"$unionWith":{"coll":` + Str(coll) + `,"pipeline":[` + stages + `]}}`)
	}
	if u.Distinct {
		var key, proj []string
		for _, c := range u.Cols {
			key = append(key, Str(c.As)+`:"$`+c.As+`"`)
			proj = append(proj, Str(c.As)+`:"$_id.`+c.As+`"`)
		}
		b.WriteString(`,{"$group":{"_id":{` + strings.Join(key, ",") + `}}},{"$project":{` + strings.Join(proj, ",") + `}}`)
	}
	var paths []string
	var desc []bool
	var nf []*bool
	for _, o := range u.OrderBy {
		c := u.Col(o.Col)
		if c == nil {
			return "", errors.New("an order by " + o.Col + ", which is not an output")
		}
		if c.Type.Array || c.Type.Name == schema.TypeJSON || c.Type.Name == schema.TypeJSONB {
			return "", errors.New("an order by an array or json column")
		}
		paths = append(paths, o.Col)
		desc = append(desc, o.Desc)
		nf = append(nf, o.NullsFirst)
	}
	b.WriteString(orderStages(paths, desc, nf))
	b.WriteString(`,{"$limit":` + Arg(len(u.Params)+1) + `}]}`)
	return b.String(), nil
}

// widens reports whether a branch's column decodes as the union's: the same
// type, or an integer or float the union widened.
func widens(from, to schema.Type) bool {
	if from.Array != to.Array {
		return false
	}
	if from.Name == to.Name || family(from) == "text" && family(to) == "text" {
		return true
	}
	switch to.Name {
	case schema.TypeInt8:
		return from.Name == schema.TypeInt2 || from.Name == schema.TypeInt4
	case schema.TypeInt4:
		return from.Name == schema.TypeInt2
	case schema.TypeFloat8:
		return from.Name == schema.TypeFloat4
	}
	return false
}
