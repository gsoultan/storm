package codegenmongo

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

func (g *gen) writes() {
	fs := g.fields()

	g.p("// Column bits, for the write masks.")
	g.p("const (")
	for i := range g.cols {
		g.p("\td%s uint64 = 1 << %d", g.field(i), i)
	}
	for j, i := range g.guards {
		g.p("\tguard%s uint64 = 1 << %d // Inc%sWhen%s", g.field(i), len(g.cols)+j, g.field(i), schema.GoName(g.cols[i].IncWhen))
	}
	g.p(")")
	g.p("")
	var km []string
	for _, k := range g.keys {
		km = append(km, "d"+g.field(k))
	}
	g.p("// keyBits are the primary key's columns.")
	g.p("const keyBits = %s", strings.Join(km, " | "))
	g.p("")
	g.p("const nKeys = %d", len(g.keys))
	g.p("")
	g.p("// setName is each column's quoted stored name, as an update document's key.")
	g.p("var setName = [nCols]string{")
	for i := range g.cols {
		if g.isKey(i) {
			g.p("\t\"\",")
			continue
		}
		g.p("\t%q,", mongo.Str(g.stored(i)))
	}
	g.p("}")
	g.p("")
	g.insertTables()
	g.pipeTables()
	g.insertFns(fs)
	g.mutFns(fs)
	g.deleteFns()
	g.insertAll()
}

// insertTables are what an insert's update document is built from.
func (g *gen) insertTables() {
	g.p("// insertDefault is the $setOnInsert literal or the $currentDate a column")
	g.p("// gets when the caller left it unset.")
	g.p("var insertDefault = [nCols][2]string{")
	for i, c := range g.cols {
		if g.isKey(i) {
			g.p("\t{},")
			continue
		}
		if i == g.soft {
			// Stored as an explicit null, not left absent: a live document
			// then has the field, which is what the live-scoped unique
			// indexes' partial filter reads.
			g.p("\t{\"$setOnInsert\", %q},", mongo.Str(g.stored(i))+":null")
			continue
		}
		switch k, lit := mongo.ClassifyDefault(c); k {
		case mongo.DefaultServerTime:
			g.p("\t{\"$currentDate\", %q},", mongo.Str(g.stored(i)))
		case mongo.DefaultLiteral:
			g.p("\t{\"$setOnInsert\", %q},", mongo.Str(g.stored(i))+":"+lit)
		default:
			g.p("\t{},")
		}
	}
	g.p("}")
	g.p("")
	g.p(`// insertDoc renders an insert's update document: set columns bound from
// hole first, the rest their defaults.
func insertDoc(set uint64, first int) string {
	var parts = map[string][]string{}
	var order []string
	add := func(o, s string) {
		if _, ok := parts[o]; !ok {
			order = append(order, o)
		}
		parts[o] = append(parts[o], s)
	}
	n := first
	for i := 0; i < nCols; i++ {
		bit := uint64(1) << i
		switch {
		case keyBits&bit != 0:
		case set&bit != 0:
			add("$setOnInsert", setName[i]+":{\"$arg\":"+itoa(n)+"}")
			n++
		case insertDefault[i][0] == "$currentDate":
			add("$currentDate", insertDefault[i][1]+":{\"$type\":\"date\"}")
		case insertDefault[i][0] != "":
			add(insertDefault[i][0], insertDefault[i][1])
		}
	}
	s := "{"
	for i, o := range order {
		if i > 0 {
			s += ","
		}
		s += "\"" + o + "\":{"
		for j, p := range parts[o] {
			if j > 0 {
				s += ","
			}
			s += p
		}
		s += "}"
	}
	if len(order) == 0 {
		s += "\"$setOnInsert\":{}"
	}
	return s + "}"
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%%10))
}
`)
}

// pipeTables are what an update's pipeline is built from.
//
// Every update is an aggregation-pipeline update — [{"$set":{...}}] — rather
// than an operator document, because three of the things a Mut can say read
// the document's OWN current value: Inc and Add (`n = n + 1` keeps a NULL a
// NULL, where $inc on a null fails), a guarded increment (`n + 1 when g = v`,
// read in the same write), and the version bump. A bound value is wrapped in
// $literal so a string that happens to start with $ is a string, not a path.
func (g *gen) pipeTables() {
	g.p("// exprSet is a column's server-side value: the server's clock for a")
	g.p("// timestamp, one more than itself for a counter.")
	g.p("var exprSet = [nCols]string{")
	for i, c := range g.cols {
		f := mongo.Str(g.stored(i))
		switch {
		case g.isKey(i):
			g.p("\t\"\",")
		case c.Type.Name == schema.TypeTimestamptz && !c.Type.Array:
			g.p("\t%q,", f+`:"$$NOW"`)
		case g.kind[i].counter:
			g.p("\t%q,", f+`:{"$add":[`+mongo.Str("$"+g.stored(i))+`,1]}`)
		default:
			g.p("\t\"\",")
		}
	}
	g.p("}")
	g.p("")
	g.p("// addOpen opens Add's form: the column plus a bound amount.")
	g.p("var addOpen = [nCols]string{")
	for i := range g.cols {
		if g.kind[i].counter && !g.isKey(i) {
			g.p("\t%q,", mongo.Str(g.stored(i))+`:{"$add":[`+mongo.Str("$"+g.stored(i))+`,{"$literal":`)
			continue
		}
		g.p("\t\"\",")
	}
	g.p("}")
	g.p("")
	if len(g.guards) > 0 {
		g.p("// guardOpen is each guarded increment, up to the value it compares with.")
		g.p("var guardOpen = [%d]string{", len(g.guards))
		for _, i := range g.guards {
			f := g.stored(i)
			w := g.stored(g.index(g.cols[i].IncWhen))
			g.p("\t%q,", mongo.Str(f)+`:{"$add":[`+mongo.Str("$"+f)+`,{"$cond":[{"$eq":[`+mongo.Str("$"+w)+`,{"$literal":`)
		}
		g.p("}")
		g.p("")
	}
	bump := ""
	if g.version >= 0 {
		f := g.stored(g.version)
		bump = mongo.Str(f) + `:{"$add":[` + mongo.Str("$"+f) + `,1]}`
	}
	g.p("// versionBump moves the version on every update; empty without one.")
	g.p("const versionBump = %q", bump)
	g.p("")
	g.p(`// pipeDoc renders an update pipeline: bound values from hole first, in
// column order, then the guarded increments' values.
func pipeDoc(dirty, expr uint64, first int) string {
	s := "[{\"$set\":{"
	n, k := first, 0
	add := func(p string) {
		if k > 0 {
			s += ","
		}
		s += p
		k++
	}
	hole := func() string {
		h := "{\"$arg\":" + itoa(n) + "}"
		n++
		return h
	}
	for i := 0; i < nCols; i++ {
		bit := uint64(1) << i
		d, e := dirty&bit != 0, expr&bit != 0
		switch {
		case keyBits&bit != 0:
		case d && e:
			add(addOpen[i] + hole() + "}]}")
		case d:
			add(setName[i] + ":{\"$literal\":" + hole() + "}")
		case e:
			add(exprSet[i])
		}
	}`)
	if len(g.guards) > 0 {
		g.p(`	for j := range guardOpen {
		if expr&(1<<(nCols+j)) != 0 {
			add(guardOpen[j] + hole() + "}]},1,0]}]}")
		}
	}`)
	}
	g.p(`	if versionBump != "" {
		add(versionBump)
	}
	return s + "}}]"
}
`)
}

func (g *gen) keySig() (sig, call string) {
	var s, c []string
	for _, k := range g.keys {
		n := lowerFirst(g.field(k))
		s = append(s, n+" "+g.kind[k].goType)
		c = append(c, n)
	}
	return strings.Join(s, ", "), strings.Join(c, ", ")
}

// lowerFirst is a Go name as a parameter: its leading initialism lowered
// whole — ID is id, URLPath urlPath — and a keyword suffixed.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	n := 0
	for n < len(s) && s[n] >= 'A' && s[n] <= 'Z' {
		n++
	}
	if n > 1 && n < len(s) {
		n-- // the last capital starts the next word
	}
	if n == 0 {
		n = 1
	}
	r := strings.ToLower(s[:n]) + s[n:]
	switch r {
	case "type", "func", "map", "range", "select", "go", "var", "default", "case", "chan", "const",
		"continue", "defer", "else", "fallthrough", "for", "goto", "if", "import", "interface",
		"package", "return", "struct", "switch", "break":
		return r + "_"
	}
	return r
}

func (g *gen) insertFns(fs []string) {
	g.p("var (")
	g.p("\tinsertPre  = %q", `{"findAndModify":`+mongo.Str(g.t.Name)+`,"query":{"_id":`+mongo.KeyValue(g.t, 1)+`,"$nor":[{}]},"update":`)
	g.p("\tinsertPost = %q", `,"upsert":true,"new":true,"fields":`+mongo.Projection(fs)+`}`)
	g.p(")")
	g.p("")
	g.p(`func insertStmt(set uint64) *runtime.Stmt {
	k := runtime.MaskKey{Dirty: set}
	if st := insertCache.Get(k); st != nil {
		return st
	}
	st := &runtime.Stmt{SQL: insertPre + insertDoc(set, nKeys+1) + insertPost}
	return insertCache.Put(k, st)
}

// Inserts reports how many distinct insert column sets have compiled.
func Inserts() int { return insertCache.Masks() }

// Ins stages a new row, with a setter for every column. A column left unset
// gets its default — now() from the SERVER's clock — or is absent, which
// reads back as NULL.
type Ins struct {
	row Row
	set uint64
}

// Create starts an insert.
func Create() Ins { return Ins{} }

// Assigned reports the columns set so far.
func (n Ins) Assigned() uint64 { return n.set }
`)
	for i := range g.cols {
		g.p("func (n *Ins) Set%s(v %s) { n.row.%s = %s; n.set |= d%s }",
			g.field(i), g.kind[i].goType, g.field(i), g.wrap(i, "v"), g.field(i))
	}
	g.p("")
	g.p("// key fills in a client-side key when the caller set none.")
	g.p("func (n *Ins) key() error {")
	for _, k := range g.keys {
		f := g.field(k)
		g.p("\tif n.set&d%s == 0 {", f)
		switch d, _ := mongo.ClassifyDefault(g.cols[k]); d {
		case mongo.DefaultUUIDv7:
			g.p("\t\tn.row.%s = runtime.NewUUIDv7()", f)
			g.p("\t\tn.set |= d%s", f)
		case mongo.DefaultUUIDv4:
			g.p("\t\t// gen_random_uuid() is a version 4 uuid; MongoDB has no generator a")
			g.p("\t\t// write can call, so it is made here, as on MySQL.")
			g.p("\t\tn.row.%s = runtime.NewUUIDv4()", f)
			g.p("\t\tn.set |= d%s", f)
		default:
			g.p("\t\treturn errors.New(%q)", "storm: "+g.t.Name+": the key has no default, so an insert must set "+f)
		}
		g.p("\t}")
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")
	var keyArgs []string
	for _, k := range g.keys {
		keyArgs = append(keyArgs, "n.row."+g.field(k))
	}
	g.p(`// Insert writes the staged row and reads it back, so server-side defaults
// land in the returned Row. A duplicate key or unique field is
// runtime.ErrUniqueViolation.
func (n *Ins) Insert(ctx context.Context, ex runtime.Executor) (Row, error) {
	if err := n.key(); err != nil {
		return Row{}, err
	}
	st := insertStmt(n.set)
	args := make([]any, 0, nCols)
	args = append(args, %s)
	args = n.row.values(args, n.set&^keyBits)
	return readBack(ctx, ex, st.SQL, args)
}

// Insert writes every column of r, zero values included, and reads the row
// back into r. A zero key is filled in when the key has a default.
func Insert(ctx context.Context, ex runtime.Executor, r *Row) error {
	n := Ins{row: *r, set: 1<<nCols - 1}`, strings.Join(keyArgs, ", "))
	for _, k := range g.keys {
		if z := g.zeroKey(k, "r."); z != "false" {
			g.p("\tif %s {", z)
			g.p("\t\tn.set &^= d%s", g.field(k))
			g.p("\t}")
		}
	}
	g.p(`	out, err := n.Insert(ctx, ex)
	if err != nil {
		return err
	}
	*r = out
	return nil
}

func readBack(ctx context.Context, ex runtime.Executor, cmd string, args []any) (Row, error) {
	var out Row
	rows, err := ex.Query(ctx, cmd, args)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return out, err
		}
		return out, runtime.ErrNoRow
	}
	if err := scan(rows.RawValues(), &out); err != nil {
		return out, err
	}
	return out, rows.Err()
}

// orNil binds a nil slice as null, which is what it means in a Row.
func orNil[T any](v []T) any {
	if v == nil {
		return nil
	}
	return v
}

// nullable binds a Null as its value, or nil.
func nullable[T any](n runtime.Null[T]) any {
	if !n.Valid {
		return nil
	}
	return n.V
}
`)
	g.p("// values appends the bound value of every column in mask, in column order.")
	g.p("// A Null that is not Valid binds null.")
	g.p("func (r *Row) values(args []any, mask uint64) []any {")
	for i := range g.cols {
		g.p("\tif mask&d%s != 0 {", g.field(i))
		g.p("\t\targs = append(args, %s)", g.unwrap(i, "r."+g.field(i)))
		g.p("\t}")
	}
	g.p("\treturn args")
	g.p("}")
	g.p("")
}

func (g *gen) mutFns(fs []string) {
	query := mongo.KeyValue(g.t, 1)
	q := `{"_id":` + query
	if g.version >= 0 {
		q += `,` + mongo.Str(g.stored(g.version)) + `:` + mongo.Arg(len(g.keys)+1)
	}
	if g.soft >= 0 {
		q += `,` + mongo.Str(g.stored(g.soft)) + `:null`
	}
	q += `}`
	first := len(g.keys) + 1
	if g.version >= 0 {
		first++
	}
	g.p("var (")
	g.p("\tupdatePre  = %q", `{"findAndModify":`+mongo.Str(g.t.Name)+`,"query":`+q+`,"update":`)
	g.p("\tupdatePost = %q", `,"new":true,"fields":`+mongo.Projection(fs)+`}`)
	g.p(")")
	g.p("")
	g.p("// Mut stages an update. Only assigned columns are written.")
	g.p("type Mut struct {")
	g.p("\trow   Row")
	g.p("\tdirty uint64")
	g.p("\texpr  uint64")
	for _, i := range g.guards {
		w := g.index(g.cols[i].IncWhen)
		g.p("\t// when%s is what Inc%sWhen%s compares %s with.", g.field(i), g.field(i), g.field(w), g.cols[w].Name)
		g.p("\twhen%s %s", g.field(i), g.kind[w].goType)
	}
	g.p("}")
	g.p("")
	g.p("// Mutate stages an update of a row read from the database.")
	g.p("func Mutate(r Row) Mut { return Mut{row: r} }")
	g.p("")
	sig, _ := g.keySig()
	if g.version < 0 {
		var set []string
		for _, k := range g.keys {
			set = append(set, g.field(k)+": "+lowerFirst(g.field(k)))
		}
		g.p("// MutateKey stages an update addressed by key alone.")
		g.p("func MutateKey(%s) Mut { return Mut{row: Row{%s}} }", sig, strings.Join(set, ", "))
	} else {
		g.p("// There is no MutateKey on this collection. The version column makes")
		g.p("// every update an optimistic lock, and a lock needs the version that")
		g.p("// was READ.")
	}
	g.p("")
	g.p("// Row returns the staged values — after Update, the row as stored.")
	g.p("func (m Mut) Row() Row { return m.row }")
	g.p("")
	g.p("// Setters. There is none for the key, an Immutable column or the version")
	g.p("// column: the absence of the method is the enforcement.")
	for i, c := range g.cols {
		if g.isKey(i) || c.Immutable || i == g.version {
			continue
		}
		f := g.field(i)
		clear := ""
		if c.IncWhen != "" {
			clear = "; m.expr &^= guard" + f
		}
		g.p("func (m *Mut) Set%s(v %s) { m.row.%s = %s; m.dirty |= d%s; m.expr &^= d%s%s }",
			f, g.kind[i].goType, f, g.wrap(i, "v"), f, f, clear)
		if g.kind[i].nullable(c) {
			g.p("func (m *Mut) Set%sNull() { m.row.%s = runtime.Null[%s]{}; m.dirty |= d%s; m.expr &^= d%s%s }",
				f, f, g.kind[i].goType, f, f, clear)
		}
		if c.Type.Name == schema.TypeTimestamptz && !c.Type.Array {
			g.p("// Set%sNow assigns the server's clock, not this process's.", f)
			g.p("func (m *Mut) Set%sNow() { m.expr |= d%s; m.dirty &^= d%s }", f, f, f)
		}
		if g.kind[i].counter {
			g.p("// Inc%s adds one to the column's own value, in the database; a null", f)
			g.p("// stays null, as it does in SQL.")
			g.p("func (m *Mut) Inc%s() { m.expr |= d%s; m.dirty &^= d%s%s }", f, f, f, clear)
			if c.NotNull {
				g.p("// Add%s adds n to the column's OWN value, in the database. Until the", f)
				g.p("// write, Row() holds n, not the column's value.")
				g.p("func (m *Mut) Add%s(n %s) { m.row.%s = n; m.dirty |= d%s; m.expr |= d%s%s }", f, g.kind[i].goType, f, f, f, clear)
			}
		}
		if c.IncWhen != "" {
			w := g.index(c.IncWhen)
			wf := g.field(w)
			g.p("// Inc%sWhen%s adds one to the column's OWN value when %s, as the", f, wf, c.IncWhen)
			g.p("// document stands before the write, equals v, and nothing otherwise.")
			g.p("// The test and the write are one command, so two writers racing see")
			g.p("// one outcome each.")
			g.p("func (m *Mut) Inc%sWhen%s(v %s) { m.when%s = v; m.dirty &^= d%s; m.expr &^= d%s; m.expr |= guard%s }",
				f, wf, g.kind[w].goType, f, f, f, f)
		}
	}
	g.p("")
	var keyArgs []string
	for _, k := range g.keys {
		keyArgs = append(keyArgs, "m.row."+g.field(k))
	}
	if g.version >= 0 {
		keyArgs = append(keyArgs, "m.row."+g.field(g.version))
	}
	var guardArgs []string
	for _, i := range g.guards {
		guardArgs = append(guardArgs, "\tif m.expr&guard"+g.field(i)+" != 0 {\n\t\targs = append(args, m.when"+g.field(i)+")\n\t}")
	}
	noRow := "runtime.ErrNoRow"
	doc := "Update writes the assigned columns of the row with this key and reads the\n// row back into the Mut. A key that matches nothing is runtime.ErrNoRow."
	if g.version >= 0 {
		noRow = "runtime.ErrStaleWrite"
		doc = "Update writes the assigned columns and reads the row back into the Mut.\n// The version column makes it an optimistic lock: a row no longer at the\n// version that was read is runtime.ErrStaleWrite. Re-read and retry."
	}
	if g.soft >= 0 {
		doc += "\n// A deleted row is not updatable: every read here says it is gone."
	}
	g.p(`// args binds a Mut's values: those it assigned, in column order, then the
// values its guarded increments compare with.
func (m *Mut) args(args []any) []any {
	args = m.row.values(args, m.dirty&^keyBits)
%s
	return args
}

// %s
// Assigning nothing sends nothing.
func (m *Mut) Update(ctx context.Context, ex runtime.Executor) error {
	if m.dirty|m.expr == 0 {
		return nil
	}
	k := runtime.MaskKey{Dirty: m.dirty, Expr: m.expr}
	st := mutCache.Get(k)
	if st == nil {
		st = mutCache.Put(k, &runtime.Stmt{SQL: updatePre + pipeDoc(m.dirty, m.expr, %d) + updatePost})
	}
	args := make([]any, 0, nCols+%d)
	args = append(args, %s)
	args = m.args(args)
	out, err := readBack(ctx, ex, st.SQL, args)
	if errors.Is(err, runtime.ErrNoRow) {
		return %s
	}
	if err != nil {
		return err
	}
	m.row = out
	m.dirty, m.expr = 0, 0
	return nil
}

// UpdateAll applies m's assignments to every matching row and reports how
// many matched.
// The query needs a predicate (runtime.ErrUnfilteredWrite) and no Limit or
// Offset (runtime.ErrPagedWrite).
func (q Query) UpdateAll(ctx context.Context, ex runtime.Executor, m Mut) (int64, error) {
	if err := q.writeAllErr(); err != nil {
		return 0, err
	}
	if m.dirty|m.expr == 0 {
		return 0, nil
	}
	var key [maxToks + 4]runtime.Tok
	toks := append(key[:0], q.stream()...)
	toks = append(toks, runtime.Tok(m.dirty), runtime.Tok(m.dirty>>32), runtime.Tok(m.expr), runtime.Tok(m.expr>>32))
	st := updateCache[0].Get(toks)
	if st == nil {
		args := m.args(nil)
		f, np, err := filter(q.stream(), len(args)+1, 0)
		st = &runtime.Stmt{Err: err, NArg: len(args) + np}
		if err == nil {
			st.SQL = %s + pipeDoc(m.dirty, m.expr, 1) + %s + f + %s
		}
		st = updateCache[0].Put(toks, st)
	}
	if st.Err != nil {
		return 0, st.Err
	}
	args := m.args(make([]any, 0, nCols+int(q.nv)))
	args = append(args, q.vals[:q.nv]...)
	return ex.Exec(ctx, st.SQL, args)
}
`, strings.Join(guardArgs, "\n"), doc, first, len(g.guards)+len(keyArgs), strings.Join(keyArgs, ", "), noRow,
		strconv.Quote(`{"update":`+mongo.Str(g.t.Name)+`,"updates":[{"multi":true,"u":`),
		strconv.Quote(`,"q":`), strconv.Quote(mongo.UpdateTail))
}

func (g *gen) deleteFns() {
	g.cascadeVar()
	sig, call := g.keySig()
	if g.soft < 0 {
		g.p("var deleteOne = %q", mongo.DeleteOne(g.t))
		g.p("")
		g.p("// Delete deletes the row with this key. A key that matches nothing is")
		g.p("// runtime.ErrNoRow.")
		if g.plan != nil {
			g.p("//")
			g.p("// The references to it are acted on first, as declared: see cascade.")
		}
		g.p("func Delete(ctx context.Context, ex runtime.Executor, %s) error {", sig)
		g.p("\treturn exactlyOne(%s)", g.removeCall("deleteOne", call))
		g.p("}")
	} else {
		sd := mongo.Str(g.stored(g.soft))
		key := mongo.KeyValue(g.t, 1)
		mark := `{"update":` + mongo.Str(g.t.Name) + `,"updates":[{"q":{"_id":` + key + `,` + sd + `:null},"u":` + g.markDoc() + `}]}`
		restore := `{"update":` + mongo.Str(g.t.Name) + `,"updates":[{"q":{"_id":` + key + `,` + sd + `:{"$ne":null}},"u":[{"$set":{` + sd + `:null}}]}]}`
		g.p("var (")
		g.p("\tsoftDeleteOne = %q", mark)
		g.p("\trestoreOne    = %q", restore)
		g.p("\thardDeleteOne = %q", mongo.DeleteOne(g.t))
		g.p(")")
		g.p("")
		g.p(`// Delete marks one row deleted with the server's clock. The document stays
// and keeps its key; every read generated in this package stops returning
// it. A row that was already deleted is runtime.ErrNoRow, not success — the
// same answer a hard delete gives for a row that is not there.
func Delete(ctx context.Context, ex runtime.Executor, %s) error {
	return exactlyOne(ex.Exec(ctx, softDeleteOne, []any{%s}))
}

// HardDelete removes the document for real, deleted or not. It is the only
// function in this package that destroys data, which is why it is not
// spelled Delete. The references to it are acted on as declared.
func HardDelete(ctx context.Context, ex runtime.Executor, %s) error {
	return exactlyOne(%s)
}

// Restore clears the mark. A row that was not deleted is runtime.ErrNoRow.
func Restore(ctx context.Context, ex runtime.Executor, %s) error {
	return exactlyOne(ex.Exec(ctx, restoreOne, []any{%s}))
}`, sig, call, sig, g.removeCall("hardDeleteOne", call), sig, call)
	}
	g.p(`
func exactlyOne(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return runtime.ErrNoRow
	}
	return nil
}
`)
}

// removeCall is the expression that runs a delete by key: the command alone,
// or through the cascade plan.
func (g *gen) removeCall(cmd, call string) string {
	if g.plan == nil {
		return "ex.Exec(ctx, " + cmd + ", []any{" + call + "})"
	}
	return "mgcascade.Delete(ctx, ex, cascade, keyFilter, []any{" + call + "}, " + cmd + ")"
}

func (g *gen) insertAll() {
	var checks []string
	for _, k := range g.keys {
		if z := g.zeroKey(k, "rows[i]."); z != "false" {
			checks = append(checks, z)
		}
	}
	cond := "false"
	if len(checks) > 0 {
		cond = strings.Join(checks, " || ")
	}
	g.p(`// InsertAll inserts rows as documents, a thousand per round trip. There is
// no read-back, so every key must be set: a zero key is refused rather
// than stored, and no defaults apply.
func InsertAll(ctx context.Context, ex runtime.Executor, rows []Row) (int64, error) {
	for i := range rows {
		if %s {
			return 0, errors.New("storm: InsertAll: row " + itoa(i) + " has no key")
		}
	}
	return ex.CopyFrom(ctx, Collection, fields[:], &rowSource{rows: rows})
}

// rowSource walks a []Row for CopyFrom without copying it.
type rowSource struct {
	rows []Row
	i    int
	buf  []any
}

func (s *rowSource) Next() bool { s.i++; return s.i <= len(s.rows) }
func (s *rowSource) Values() []any {
	s.buf = s.rows[s.i-1].values(s.buf[:0], 1<<nCols-1)
	return s.buf
}
func (s *rowSource) Err() error { return nil }
`, cond)
}

// zeroKey is the Go condition that a key column holds its zero value, for a
// key whose zero is not a key: a uuid, or text. "false" for any other.
func (g *gen) zeroKey(i int, on string) string {
	switch g.kind[i].goType {
	case "[16]byte":
		return "runtime.IsZeroUUID(" + on + g.field(i) + ")"
	case "string":
		if d, _ := mongo.ClassifyDefault(g.cols[i]); d == mongo.DefaultNone {
			return on + g.field(i) + ` == ""`
		}
	}
	return "false"
}

// wrap turns a value of the base type into the Row field's type.
func (g *gen) wrap(i int, v string) string {
	if g.kind[i].nullable(g.cols[i]) {
		return "runtime.Null[" + g.kind[i].goType + "]{V: " + v + ", Valid: true}"
	}
	return v
}

// unwrap turns a Row field into what is bound: nil for a NULL.
func (g *gen) unwrap(i int, f string) string {
	switch {
	case g.kind[i].nullable(g.cols[i]):
		return "nullable(" + f + ")"
	case g.kind[i].array:
		return "orNil(" + f + ")"
	}
	return f
}

func (g *gen) scan() {
	g.p(`// scan decodes one document's fields, in projection order. A field of the
// wrong BSON type is an error naming it, never a plausible zero.`)
	all := make([]int, len(g.cols))
	for i := range all {
		all[i] = i
	}
	g.scanCols("scan", "Row", "nCols", all)
}

// scanCols emits fn, which decodes the columns cols — rv[j] is cols[j] — into
// a rowType.
func (g *gen) scanCols(fn, rowType, n string, cols []int) {
	g.p("func %s(rv [][]byte, r *%s) error {", fn, rowType)
	g.p("\tif len(rv) != %s {", n)
	g.p("\t\treturn runtime.ErrShape")
	g.p("\t}")
	g.p("\tvar err error")
	for j, i := range cols {
		c := g.cols[i]
		k := g.kind[i]
		f := g.field(i)
		switch {
		case k.array:
			g.p("\tif rv[%d] != nil {", j)
			g.p("\t\tif r.%s, err = bsondec.Array(rv[%d], bsondec.%s); err != nil {", f, j, k.elemDec)
			g.p("\t\t\treturn errors.Join(errors.New(%q), err)", "storm: "+g.t.Name+"."+c.Name)
			g.p("\t\t}")
			g.p("\t}")
			continue
		case k.slice:
			g.p("\tif rv[%d] != nil {", j)
			g.p("\t\tif r.%s, err = bsondec.%s(rv[%d]); err != nil {", f, k.dec, j)
			g.p("\t\t\treturn errors.Join(errors.New(%q), err)", "storm: "+g.t.Name+"."+c.Name)
			g.p("\t\t}")
			g.p("\t}")
			continue
		case k.nullable(c):
			g.p("\tif r.%s, err = bsondec.Nullable(rv[%d], bsondec.%s); err != nil {", f, j, k.dec)
		default:
			g.p("\tif r.%s, err = bsondec.%s(rv[%d]); err != nil {", f, k.dec, j)
		}
		g.p("\t\treturn errors.Join(errors.New(%q), err)", "storm: "+g.t.Name+"."+c.Name)
		g.p("\t}")
	}
	g.p("\treturn nil")
	g.p("}")
}
