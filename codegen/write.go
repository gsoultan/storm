package codegen

import (
	"fmt"

	"github.com/gsoultan/raorm/compile/pgsql"
	"github.com/gsoultan/raorm/schema"
)

// Write-path emission.
//
// The read path's insight applied to writes: an UPDATE's identity is the *set*
// of columns it assigns, not the values it assigns them. So a dirty mask picks
// SET fragments, the statement is compiled once per distinct mask, and a warm
// write is a cache probe and a bind — the same bargain, one bit per column
// instead of a token stream.
//
// maxDirty is 64 because the mask is a uint64. Reads allow far more columns
// (runtime.MaxCols), so this is checked separately rather than assumed.
const maxDirty = 64

// insertable is every column a write may supply. Generated columns are the
// database's to compute; supplying one is an error Postgres would raise anyway,
// and catching it here names the model field instead of the SQL.
func insertable(t *schema.Table) []colInfo {
	var out []colInfo
	for _, c := range t.Columns {
		if c.Generated != "" || goKind(c) == kindUnsupported {
			continue
		}
		out = append(out, colInfo{col: c, kind: goKind(c), goBase: baseGoType(c)})
	}
	return out
}

// updatable is every column an UPDATE may assign.
//
// Excluded, each for its own reason:
//   - the primary key: changing it is a delete and an insert, not an update,
//     and pretending otherwise makes referential integrity a guess
//   - Immutable columns: declared unchangeable, so there is no setter at all —
//     enforced by the type, not by a runtime check
//   - the Version column: the ORM owns it. A client that could assign it could
//     defeat the optimistic lock by writing the value it just read
func updatable(t *schema.Table) []colInfo {
	pk := map[string]bool{}
	for _, k := range t.PrimaryKey {
		pk[k] = true
	}
	var out []colInfo
	for _, c := range t.Columns {
		if c.Generated != "" || c.Immutable || c.Version || pk[c.Name] || goKind(c) == kindUnsupported {
			continue
		}
		out = append(out, colInfo{col: c, kind: goKind(c), goBase: baseGoType(c)})
	}
	return out
}

func versionCol(t *schema.Table) *schema.Column {
	for _, c := range t.Columns {
		if c.Version {
			return c
		}
	}
	return nil
}

// pkCols is what identifies a row for update and delete.
func pkCols(t *schema.Table) []colInfo {
	var out []colInfo
	for _, k := range t.PrimaryKey {
		for _, c := range t.Columns {
			if c.Name == k {
				out = append(out, colInfo{col: c, kind: goKind(c), goBase: baseGoType(c)})
			}
		}
	}
	return out
}

// writes emits the whole write surface, or nothing if the table cannot carry
// one.
func (g *gen) writes() {
	ins, upd, pk := insertable(g.t), updatable(g.t), pkCols(g.t)
	if len(pk) == 0 {
		// Without a primary key there is no way to address a row, so an update
		// or delete would either need a full-table predicate or a guess. Reads
		// still work; writes are simply not generated, and the reason is in the
		// output where whoever goes looking will find it.
		g.p("// No write API: table %s has no primary key, so no statement can", g.t.Name)
		g.p("// address a single row.")
		g.p("// Add one to the model to get Insert, Update and Delete.")
		g.p("")
		return
	}
	// A column the generator cannot supply, that the database will not supply
	// either, makes every INSERT fail at runtime with a NOT NULL violation. The
	// model is wrong and the author should hear it now, from the generator,
	// naming the column — not later, from Postgres, naming a constraint.
	if err := insertComplete(g.t, ins); err != nil {
		g.err = err
		return
	}
	if len(upd) > maxDirty {
		g.err = fmt.Errorf(
			"codegen: table %s has %d updatable columns; the dirty mask holds %d — "+
				"split the table, or mark columns Immutable",
			g.t.Name, len(upd), maxDirty)
		return
	}

	g.writeConsts(ins, upd, pk)
	g.mutType(upd)
	g.insType(ins)
	g.insertFn(ins)
	g.updateFn(upd, pk)
	g.deleteFn(pk)
}

func (g *gen) writeConsts(ins, upd, pk []colInfo) {
	// Every column comes back from an insert: the database computes defaults,
	// and reading them with a second SELECT races every other writer.
	all := make([]string, 0, len(g.t.Columns))
	for _, c := range g.t.Columns {
		if goKind(c) != kindUnsupported {
			all = append(all, c.Name)
		}
	}
	names := make([]string, len(ins))
	for i, c := range ins {
		names[i] = c.Name()
	}

	g.p("// insertSQL does not vary: the column list is fixed by the table, so")
	g.p("// the placeholders are known at build time and nothing is spliced.")
	g.p("const insertSQL = `%s`", pgsql.InsertStmt(g.t.Name, names, all))
	g.p("")
	g.p("const updatePrefix = `%s`", pgsql.UpdatePrefix(g.t.Name))
	g.p("const deletePrefix = `%s`", pgsql.DeletePrefix(g.t.Name))
	g.p("")

	g.p("// Dirty bits. One per updatable column; the set of them is an UPDATE's")
	g.p("// identity, exactly as a token stream is a SELECT's.")
	g.p("const (")
	for i, c := range upd {
		g.p("\td%s uint64 = 1 << %d", exportName(c.Name()), i)
	}
	g.p(")")
	g.p("")
	g.p("const nUpdatable = %d", len(upd))
	g.p("")

	g.p("// setFrags is every assignment this table can make, lowered at build time.")
	g.p("var setFrags = [nUpdatable]runtime.Frag{")
	for _, c := range upd {
		a, b := pgsql.SetFrag(c.Name())
		g.p("\t{A: %q, B: %q}, // %s", a, b, c.Name())
	}
	g.p("}")
	g.p("")

	g.p("// pkFrags addresses one row.")
	g.p("var pkFrags = [%d]runtime.Frag{", len(pk))
	for _, c := range pk {
		a, b, ok := pgsql.Frag("Eq", pgsql.Ident(c.Name()))
		if !ok {
			g.err = fmt.Errorf("codegen: table %s: no equality lowering for primary key column %s", g.t.Name, c.Name())
			return
		}
		g.p("\t{A: %q, B: %q}, // %s", a, b, c.Name())
	}
	g.p("}")
	g.p("")

	if v := versionCol(g.t); v != nil {
		a, b, _ := pgsql.Frag("Eq", pgsql.Ident(v.Name))
		g.p("// versionFrag is the optimistic lock. An update carrying it matches")
		g.p("// no row when somebody else wrote first, which is the whole mechanism.")
		g.p("var versionFrag = runtime.Frag{A: %q, B: %q}", a, b)
		ba, bb := pgsql.BumpFrag(v.Name)
		g.p("")
		g.p("// versionBump increments from the column's own value, never from one")
		g.p("// the client read — two writers who both saw 3 must not both write 4.")
		g.p("var versionBump = runtime.Frag{A: %q, B: %q}", ba, bb)
		g.p("")
	}
	g.p("// Insert bits. A masked INSERT names only the columns the caller")
	g.p("// assigned, so every other column takes its database default — which is")
	g.p("// the only way a DEFAULT gen_random_uuid() can ever fire.")
	g.p("const (")
	for i, c := range ins {
		g.p("\ti%s uint64 = 1 << %d", exportName(c.Name()), i)
	}
	g.p(")")
	g.p("")
	g.p("const nInsertable = %d", len(ins))
	g.p("")
	g.p("// insCols is the quoted column name for each insert bit.")
	g.p("var insCols = [nInsertable]string{")
	for _, c := range ins {
		g.p("\t%q,", pgsql.Ident(c.Name()))
	}
	g.p("}")
	g.p("")
	open_, sep, mid, close_ := pgsql.InsertParts()
	g.p("// insParts and insPlaceholder come from the back end at build time; the")
	g.p("// runtime splicer chooses none of them.")
	g.p("var insParts = runtime.InsertParts{Open: %q, Sep: %q, Mid: %q, Close: %q}", open_, sep, mid, close_)
	g.p("const insPlaceholder = %q", pgsql.Placeholder)
	g.p("const insPrefix = %q", pgsql.InsertPrefix(g.t.Name))
	g.p("const insReturning = %q", pgsql.ReturningClause(allReadable(g.t)))
	g.p("")
	g.p("var insCache = runtime.NewMaskCache()")
	g.p("var updCache = runtime.NewMaskCache()")
	g.p("")
	g.p("// Masks reports how many distinct UPDATE shapes have compiled.")
	g.p("func Masks() int { return updCache.Masks() }")
	g.p("")
}

func (g *gen) mutType(upd []colInfo) {
	g.p("// Mut is a row staged for update: the values, plus which of them were")
	g.p("// actually assigned. An UPDATE writes the assigned ones and no others,")
	g.p("// so a read-modify-write cannot clobber a column it never looked at.")
	g.p("type Mut struct {")
	g.p("\trow   Row")
	g.p("\tdirty uint64")
	g.p("}")
	g.p("")
	g.p("// Mutate stages a row read from the database.")
	g.p("func Mutate(r Row) Mut { return Mut{row: r} }")
	g.p("")
	g.p("// Row returns the staged values.")
	g.p("func (m Mut) Row() Row { return m.row }")
	g.p("")
	g.p("// Dirty reports the assigned-column mask, which is also the statement key.")
	g.p("func (m Mut) Dirty() uint64 { return m.dirty }")
	g.p("")
	g.p("// Setters. There is deliberately no setter for the primary key, for an")
	g.p("// Immutable column, or for the version column: the absence of a method is")
	g.p("// the enforcement, so misuse does not compile rather than failing later.")
	for _, c := range upd {
		n := exportName(c.Name())
		// The setter takes the bare T even when the column is nullable:
		// requiring a caller to build a Null[T] to assign a value that is
		// definitely present is ceremony for nothing.
		g.p("func (m *Mut) Set%s(v %s) {", n, c.goBase)
		g.p("\tm.row.%s = %s", n, mutAssign(c))
		g.p("\tm.dirty |= d%s", n)
		g.p("}")
		g.p("")
		if isNullable(c.col) {
			// Writing NULL needs its own method. Overloading the setter with a
			// sentinel value would make NULL and the zero value the same
			// thing, which is the bug this type system exists to prevent.
			g.p("// Set%sNull writes SQL NULL. It is a separate method because a", n)
			g.p("// zero value and an absent value are different facts.")
			g.p("func (m *Mut) Set%sNull() {", n)
			g.p("\tm.row.%s = runtime.Null[%s]{}", n, c.goBase)
			g.p("\tm.dirty |= d%s", n)
			g.p("}")
			g.p("")
		}
	}
}

// mutAssign adapts a caller's value to the row's field type. A nullable column
// reads back as Null[T] but is set from a plain T: asking a caller to build a
// Null[T] to assign a value that is definitely present is ceremony for nothing.
func mutAssign(c colInfo) string {
	if isNullable(c.col) {
		return fmt.Sprintf("runtime.Null[%s]{V: v, Valid: true}", c.goBase)
	}
	return "v"
}

// insType emits the masked insert builder.
func (g *gen) insType(ins []colInfo) {
	g.p("// Ins stages a new row. Unlike Mut it has a setter for every insertable")
	g.p("// column including the primary key and Immutable ones — supplying your")
	g.p("// own id is legitimate, changing it later is not.")
	g.p("//")
	g.p("// A column left unset is absent from the statement, so the database")
	g.p("// applies its default. Absence is tracked by the mask, never inferred")
	g.p("// from a zero value: that inference is why other ORMs cannot insert a")
	g.p("// false, a 0 or an empty string into a column that has a default.")
	g.p("type Ins struct {")
	g.p("\trow Row")
	g.p("\tset uint64")
	g.p("}")
	g.p("")
	g.p("// Create stages a new row.")
	g.p("func Create() Ins { return Ins{} }")
	g.p("")
	g.p("// Assigned reports the mask, which is also the statement key.")
	g.p("func (n Ins) Assigned() uint64 { return n.set }")
	g.p("")
	for _, c := range ins {
		nm := exportName(c.Name())
		g.p("func (n *Ins) Set%s(v %s) {", nm, c.goBase)
		g.p("\tn.row.%s = %s", nm, mutAssign(c))
		g.p("\tn.set |= i%s", nm)
		g.p("}")
		g.p("")
		if isNullable(c.col) {
			g.p("// Set%sNull writes SQL NULL explicitly, which is not the same as", nm)
			g.p("// leaving the column unset and taking its default.")
			g.p("func (n *Ins) Set%sNull() {", nm)
			g.p("\tn.row.%s = runtime.Null[%s]{}", nm, c.goBase)
			g.p("\tn.set |= i%s", nm)
			g.p("}")
			g.p("")
		}
	}

	g.p("func stmtForInsert(mask uint64) *runtime.Stmt {")
	g.p("\tif st := insCache.Get(mask); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\tcols := make([]string, 0, nInsertable)")
	g.p("\tfor i := 0; i < nInsertable; i++ {")
	g.p("\t\tif mask&(1<<uint(i)) != 0 {")
	g.p("\t\t\tcols = append(cols, insCols[i])")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn insCache.Put(mask, runtime.SpliceInsert(insPrefix, insParts, cols, insPlaceholder, insReturning))")
	g.p("}")
	g.p("")

	g.p("// Insert writes the assigned columns and reads every column back, so")
	g.p("// database-computed values land in the returned Row rather than needing a")
	g.p("// second SELECT that would race every other writer.")
	g.p("func (n *Ins) Insert(ctx context.Context, ex runtime.Executor) (Row, error) {")
	g.p("\tif n.set == 0 {")
	g.p("\t\treturn Row{}, runtime.ErrNothingAssigned")
	g.p("\t}")
	g.p("\tst := stmtForInsert(n.set)")
	g.p("\targs := make([]any, 0, st.NArg)")
	g.p("\tfor i := 0; i < nInsertable; i++ {")
	g.p("\t\tif n.set&(1<<uint(i)) == 0 {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tswitch i {")
	for i, c := range ins {
		g.p("\t\tcase %d:", i)
		g.p("\t\t\targs = append(args, %s)", writeArg(c, "n.row."+exportName(c.Name())))
	}
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tvar out Row")
	g.p("\trows, err := ex.Query(ctx, st.SQL, args)")
	g.p("\tif err != nil {")
	g.p("\t\treturn out, err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tif !rows.Next() {")
	g.p("\t\tif err := rows.Err(); err != nil {")
	g.p("\t\t\treturn out, err")
	g.p("\t\t}")
	g.p("\t\treturn out, runtime.ErrNoRow")
	g.p("\t}")
	g.p("\tvar sl runtime.Slab")
	g.p("\tscan(rows.RawValues(), &out, &sl)")
	g.p("\treturn out, rows.Err()")
	g.p("}")
	g.p("")
	g.p("// Inserts reports how many distinct INSERT column sets have compiled.")
	g.p("func Inserts() int { return insCache.Masks() }")
	g.p("")
}

func (g *gen) insertFn(ins []colInfo) {
	g.p("// Insert writes one row and reads back every column, so database-computed")
	g.p("// defaults — a generated id, a now() timestamp — land in r rather than")
	g.p("// needing a second SELECT that would race every other writer.")
	g.p("//")
	g.p("// Every insertable column is written, including zero values. raorm does")
	g.p("// not treat a zero as 'unset': that guess is why other ORMs cannot insert")
	g.p("// a false, a 0 or an empty string into a column with a default.")
	g.p("func Insert(ctx context.Context, ex runtime.Executor, r *Row) error {")
	g.p("\targs := make([]any, 0, %d)", len(ins))
	for _, c := range ins {
		g.p("\targs = append(args, %s)", writeArg(c, "r."+exportName(c.Name())))
	}
	g.p("\trows, err := ex.Query(ctx, insertSQL, args)")
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tdefer rows.Close()")
	g.p("\tif !rows.Next() {")
	g.p("\t\tif err := rows.Err(); err != nil {")
	g.p("\t\t\treturn err")
	g.p("\t\t}")
	g.p("\t\treturn runtime.ErrNoRow")
	g.p("\t}")
	g.p("\t// The returned strings live as long as this Slab, and the Slab lives")
	g.p("\t// as long as r — a Row from an insert owns its own arena, unlike a Row")
	g.p("\t// from a scan, which shares the result set's.")
	g.p("\tvar sl runtime.Slab")
	g.p("\tscan(rows.RawValues(), r, &sl)")
	g.p("\treturn rows.Err()")
	g.p("}")
	g.p("")
}

func (g *gen) updateFn(upd, pk []colInfo) {
	hasVersion := versionCol(g.t) != nil

	g.p("// stmtForMask compiles the UPDATE for one dirty mask, once.")
	g.p("func stmtForMask(mask uint64) *runtime.Stmt {")
	g.p("\tif st := updCache.Get(mask); st != nil {")
	g.p("\t\treturn st")
	g.p("\t}")
	g.p("\tset := make([]runtime.Frag, 0, nUpdatable+1)")
	g.p("\tfor i := 0; i < nUpdatable; i++ {")
	g.p("\t\tif mask&(1<<uint(i)) != 0 {")
	g.p("\t\t\tset = append(set, setFrags[i])")
	g.p("\t\t}")
	g.p("\t}")
	if hasVersion {
		g.p("\tset = append(set, versionBump)")
	}
	g.p("\twhere := make([]runtime.Frag, 0, %d)", len(pk)+1)
	g.p("\twhere = append(where, pkFrags[:]...)")
	if hasVersion {
		g.p("\twhere = append(where, versionFrag)")
	}
	g.p("\treturn updCache.Put(mask, runtime.SpliceSections(updatePrefix, []runtime.Section{")
	g.p("\t\t{Lead: %q, Sep: %q, Frags: set},", pgsql.SetLead, pgsql.SetSep)
	g.p("\t\t{Lead: %q, Sep: %q, Frags: where},", pgsql.WhereLead, pgsql.WhereSep)
	g.p("\t}, \"\"))")
	g.p("}")
	g.p("")

	g.p("// Update writes the assigned columns of one row.")
	g.p("//")
	if hasVersion {
		g.p("// The version column makes this an optimistic lock: the statement matches")
		g.p("// only a row still at the version that was read, and a miss is")
		g.p("// runtime.ErrStaleWrite rather than a silent no-op. Re-read and retry;")
		g.p("// do not force it, because the update was computed from a value that is")
		g.p("// no longer true.")
	}
	g.p("// Assigning nothing is not an error and issues no statement — an UPDATE")
	g.p("// with an empty SET list is not valid SQL, and a caller looping over")
	g.p("// possibly-changed fields should not have to special-case the empty case.")
	g.p("func (m *Mut) Update(ctx context.Context, ex runtime.Executor) error {")
	g.p("\tif m.dirty == 0 {")
	g.p("\t\treturn nil")
	g.p("\t}")
	g.p("\tst := stmtForMask(m.dirty)")
	g.p("\targs := make([]any, 0, st.NArg)")
	g.p("\tfor i := 0; i < nUpdatable; i++ {")
	g.p("\t\tif m.dirty&(1<<uint(i)) == 0 {")
	g.p("\t\t\tcontinue")
	g.p("\t\t}")
	g.p("\t\tswitch i {")
	for i, c := range upd {
		g.p("\t\tcase %d:", i)
		g.p("\t\t\targs = append(args, %s)", writeArg(c, "m.row."+exportName(c.Name())))
	}
	g.p("\t\t}")
	g.p("\t}")
	for _, c := range pk {
		g.p("\targs = append(args, %s)", writeArg(c, "m.row."+exportName(c.Name())))
	}
	if v := versionCol(g.t); v != nil {
		g.p("\targs = append(args, m.row.%s)", exportName(v.Name))
	}
	g.p("\tn, err := ex.Exec(ctx, st.SQL, args)")
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tif n == 0 {")
	if hasVersion {
		g.p("\t\treturn runtime.ErrStaleWrite")
	} else {
		g.p("\t\treturn runtime.ErrNoRow")
	}
	g.p("\t}")
	g.p("\tm.dirty = 0")
	if v := versionCol(g.t); v != nil {
		g.p("\tm.row.%s++ // the database incremented it; keep the staged row usable", exportName(v.Name))
	}
	g.p("\treturn nil")
	g.p("}")
	g.p("")
}

func (g *gen) deleteFn(pk []colInfo) {
	g.p("var deleteSQL = runtime.SpliceSections(deletePrefix, []runtime.Section{")
	g.p("\t{Lead: %q, Sep: %q, Frags: pkFrags[:]},", pgsql.WhereLead, pgsql.WhereSep)
	g.p("}, \"\").SQL")
	g.p("")
	g.p("// Delete removes one row by primary key. A row that was already gone is")
	g.p("// runtime.ErrNoRow, not success: a caller deleting something that is not")
	g.p("// there usually has a bug, and swallowing it hides the bug.")
	sig := make([]string, 0, len(pk))
	call := make([]string, 0, len(pk))
	for _, c := range pk {
		sig = append(sig, fmt.Sprintf("%s %s", lowerFirst(exportName(c.Name())), c.goBase))
		call = append(call, lowerFirst(exportName(c.Name())))
	}
	g.p("func Delete(ctx context.Context, ex runtime.Executor, %s) error {", joinStr(sig, ", "))
	g.p("\tn, err := ex.Exec(ctx, deleteSQL, []any{%s})", joinStr(call, ", "))
	g.p("\tif err != nil {")
	g.p("\t\treturn err")
	g.p("\t}")
	g.p("\tif n == 0 {")
	g.p("\t\treturn runtime.ErrNoRow")
	g.p("\t}")
	g.p("\treturn nil")
	g.p("}")
	g.p("")
}

// writeArg unwraps a nullable field for binding: the driver wants the value or
// nil, not a Null[T] it has never heard of.
func writeArg(c colInfo, expr string) string {
	if isNullable(c.col) {
		return expr + ".Arg()"
	}
	return expr
}

func joinStr(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

// insertComplete reports a column no INSERT could satisfy: NOT NULL, no
// database default, and of a type this generator cannot bind.
func insertComplete(t *schema.Table, ins []colInfo) error {
	have := make(map[string]bool, len(ins))
	for _, c := range ins {
		have[c.Name()] = true
	}
	for _, c := range t.Columns {
		if have[c.Name] || c.Generated != "" || !c.NotNull || c.Default != "" {
			continue
		}
		return fmt.Errorf(
			"codegen: table %s column %s is %s NOT NULL with no default, and raorm "+
				"cannot bind that type yet — every INSERT would fail. Give it a "+
				"default in the model, or make it nullable",
			t.Name, c.Name, c.Type.SQL())
	}
	return nil
}

// allReadable is every column a scan projects — what an insert returns.
func allReadable(t *schema.Table) []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		if goKind(c) != kindUnsupported {
			out = append(out, c.Name)
		}
	}
	return out
}
