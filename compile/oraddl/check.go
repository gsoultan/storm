package oraddl

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Check reports every portability problem in one pass — all of them, not the
// first: finding them one deploy at a time is the failure mode this package
// exists to replace.
func Check(s *schema.Schema) error {
	var problems []string
	enums := enumsOf(s)

	for _, t := range s.Tables {
		checkIdent(t.Pos, "table", t.Name, &problems)
		if len(t.Excludes) > 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: EXCLUDE constraints have no Oracle equivalent — the overlap they "+
					"prevent becomes a race the application cannot win", at(t.Pos), t.Name))
		}
		if t.Partition != nil || t.PartitionOf != "" {
			// Oracle HAS partitioning, and it is an extra-cost option on some
			// editions and a different syntax on all of them. Refusing beats
			// emitting DDL that works on the vendor's database and is a
			// licensing question on the customer's.
			problems = append(problems, fmt.Sprintf(
				"  %s%s: storm does not generate Oracle partitioning — the syntax differs "+
					"from PostgreSQL's and partitioning is a licensed option on some editions",
				at(t.Pos), t.Name))
		}

		for _, c := range t.Columns {
			checkIdent(colPos(t, c), "column", c.Name, &problems)
			if c.Type.Enum {
				if _, ok := enums[c.Type.Name]; !ok {
					problems = append(problems, fmt.Sprintf(
						"  %s%s.%s: enum %s is not declared in the schema",
						at(colPos(t, c)), t.Name, c.Name, c.Type.Name))
				}
			} else if _, err := TypeSQL(t.Name, c); err != nil {
				problems = append(problems, "  "+at(colPos(t, c))+err.Error())
			}
			checkEmptyString(t, c, &problems)
			if why := checkDefault(c); why != "" {
				problems = append(problems, fmt.Sprintf("  %s%s.%s %s",
					at(colPos(t, c)), t.Name, c.Name, why))
			}
		}

		if len(t.PrimaryKey) == 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: a table with no primary key has nothing for a foreign key to "+
					"reference and nothing for storm's write path to identify a row by",
				at(t.Pos), t.Name))
		}
		for _, fk := range t.ForeignKeys {
			checkForeignKey(t, fk, &problems)
		}
		for _, ix := range t.Indexes {
			checkIndex(t, ix, &problems)
		}
	}

	for _, e := range s.Enums {
		checkIdent("", "enum", e.Name, &problems)
	}
	if len(s.Functions) > 0 || len(s.Views) > 0 || len(s.Triggers) > 0 {
		problems = append(problems, "  routines: storm does not generate Oracle functions, "+
			"views or triggers — PL/SQL is a different language from PL/pgSQL and storm "+
			"will not translate one into the other")
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this model does not port to Oracle:\n%s", strings.Join(problems, "\n"))
}

// checkEmptyString is the rule the whole Oracle milestone turns on.
//
// docs/PLAN.md made the capability model's ability to carry empty-string-is-NULL
// the KILL CRITERION for M12, and internal/oraclespike measured what that
// actually requires. The finding, in full, because it is not obvious:
//
// docs/DIALECTS.md used to claim that declaring oracle made "any Eq("") or
// non-null-constrained text column a declare-time error". THE Eq("") HALF IS
// IMPOSSIBLE. storm has two DSLs that carry a value: the expression DSL folds a
// Go literal into a schema.Literal at build time, but the generated query DSL's
// `func (h TextCol) Eq(v string) Pred` takes a RUNTIME string and binds it. The
// generator never sees the value, and the schema a Check receives has tables and
// columns in it and no queries at all.
//
// THE HALF THAT SURVIVES IS ENOUGH, and the load-bearing measurement is
// ORA-01400: an empty string reaching a NOT NULL column is an ERROR. So the
// difference is silent in exactly ONE place — a nullable text column, where ""
// and NULL become the same stored value — and refusing that column makes
// everything else consistent:
//
//   - nothing can BE "", so Eq("") matching nothing is the RIGHT answer rather
//     than a wrong one, which is why the impossible half never needed to work;
//   - writing "" to a required column raises ORA-01400 rather than storing NULL;
//   - the IR's function registry has lower, upper, abs, coalesce and nullif and
//     no SUBSTR, so an empty string produced by an EXPRESSION — the one leak a
//     column rule cannot see — is unreachable through the model DSL. It is
//     reachable through storm.SQL, which is the escape hatch and is outside the
//     capability model by the rule that already puts it there.
func checkEmptyString(t *schema.Table, c *schema.Column, problems *[]string) {
	if !isText(c.Type) || c.Generated != "" {
		return
	}
	if !c.NotNull {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s.%s: a NULLABLE text column cannot round-trip on Oracle — an empty "+
				"string is stored as NULL, so \"\" and nil are one value\n"+
				"      make it NOT NULL, or move it to a table whose absence means absent",
			at(colPos(t, c)), t.Name, c.Name))
	}
	if isEmptyLiteral(c.Default) {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s.%s: DEFAULT '' is DEFAULT NULL on Oracle, which is a different column",
			at(colPos(t, c)), t.Name, c.Name))
	}
}

func isText(t schema.Type) bool {
	return !t.Array && (t.Name == schema.TypeText || t.Name == schema.TypeVarchar || t.Enum)
}

// isEmptyLiteral spots ” written as a default. Not a parser: a default is raw
// SQL and these are the only spellings that mean the empty string.
func isEmptyLiteral(def string) bool {
	d := strings.TrimSpace(def)
	return d == "''" || strings.EqualFold(d, "N''")
}

func checkDefault(c *schema.Column) string {
	switch strings.ToLower(strings.TrimSpace(c.Default)) {
	case "uuidv7()":
		// The same refusal msddl makes, for the same reason and with a worse
		// alternative. Oracle's SYS_GUID() is not random at all — it is
		// documented as host-and-sequence derived — so it is neither a v4 nor
		// a v7, and substituting it would put a guessable, non-time-ordered
		// value where the model asked for a time-ordered one.
		return "asks for uuidv7(), which Oracle has no function for; SYS_GUID() is " +
			"host-and-sequence derived rather than random or time-ordered\n" +
			"      generate the key client-side, or use gen_random_uuid() and accept SYS_GUID()"
	}
	return ""
}

func checkForeignKey(t *schema.Table, fk *schema.ForeignKey, problems *[]string) {
	checkIdent(t.Pos, "constraint", fk.Name, problems)
	// ON UPDATE does not exist here. Not "spelled differently" — Oracle has no
	// ON UPDATE clause on a foreign key, and the only way to get the behaviour
	// is a trigger storm will not write. A constraint quietly weaker than the
	// model said is exactly what this package refuses.
	if fk.OnUpdate != "" && fk.OnUpdate != schema.NoAction && fk.OnUpdate != schema.Restrict {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s.%s: ON UPDATE %s has no Oracle form — there is no ON UPDATE clause on "+
				"a foreign key at all, and emitting the constraint without it would be "+
				"weaker than the model says\n"+
				"      drop the ON UPDATE, or keep the key immutable so it never fires",
			at(t.Pos), t.Name, fk.Name, fk.OnUpdate))
	}
	if fk.OnDelete == schema.SetDefault {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s.%s: ON DELETE SET DEFAULT has no Oracle form; CASCADE and SET NULL "+
				"are the only two", at(t.Pos), t.Name, fk.Name))
	}
}

func checkIndex(t *schema.Table, ix *schema.Index, problems *[]string) {
	pos := t.Pos
	if len(ix.Columns) > 0 {
		if c := t.Column(ix.Columns[0].Name); c != nil && c.Pos != "" {
			pos = c.Pos
		}
	}
	name := indexName(t, ix)
	checkIdent(pos, "index", name, problems)

	if ix.Method != "" && ix.Method != "btree" {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s asks for method %s; Oracle's ordinary index is a B-tree and "+
				"its other kinds (bitmap, domain) are not interchangeable with it",
			at(pos), t.Name, name, ix.Method))
	}
	if len(ix.Include) > 0 {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s has INCLUDE columns; Oracle has no covering-index clause — "+
				"add them as key columns instead, which changes the ordering it can serve",
			at(pos), t.Name, name))
	}
	if ix.NullsNotDistinct {
		// Oracle's unique treats NULLs as DISTINCT, like PostgreSQL's default.
		// A model asking for the opposite is asking for something neither
		// engine's plain unique does.
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s asks for NULLS NOT DISTINCT; Oracle's unique treats NULLs as "+
				"distinct and has no switch\n"+
				"      add a generated column that maps NULL to a sentinel and index that",
			at(pos), t.Name, name))
	}
	if len(ix.With) > 0 {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s carries storage parameters, which are PostgreSQL's spelling "+
				"and mean nothing here", at(pos), t.Name, name))
	}
	// A partial index becomes a function-based one, which is exact for UNIQUE
	// (an all-NULL key is not indexed, so the same rows are constrained) and
	// only an approximation for a plain index — it would still be scanned for
	// rows the predicate excludes. Refusing the plain case is the honest cut.
	if ix.Where != "" && !ix.Unique {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s is partial and not unique; Oracle has no WHERE on an index, "+
				"and the CASE expression that makes a partial UNIQUE exact does not make a "+
				"partial lookup index faster\n"+
				"      drop the WHERE, or make the index unique if that is what it is for",
			at(pos), t.Name, name))
	}
	for _, c := range ix.Columns {
		if c.NullsFirst || c.NullsLast {
			*problems = append(*problems, fmt.Sprintf(
				"  %s%s: index %s places NULLs explicitly; Oracle sorts NULLs last ascending "+
					"and has no NULLS FIRST/LAST in an index definition", at(pos), t.Name, name))
			break
		}
	}
	for _, c := range ix.Columns {
		if c.OpClass != "" || c.Collate != "" || c.Prefix > 0 {
			*problems = append(*problems, fmt.Sprintf(
				"  %s%s: index %s uses an operator class, a collation or a prefix length; "+
					"none has an Oracle form", at(pos), t.Name, name))
			break
		}
	}
}

// checkIdent enforces Oracle's identifier limit, which storm can exceed on its
// own: a derived name like ck_<table>_<column> is as long as the two names it
// joins, and nothing else storm targets has a limit this low.
func checkIdent(pos, kind, name string, problems *[]string) {
	if len([]rune(name)) > maxIdent {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s name %q is %d characters; Oracle's limit is %d",
			at(pos), kind, name, len([]rune(name)), maxIdent))
	}
}
