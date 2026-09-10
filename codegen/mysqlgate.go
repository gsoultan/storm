package codegen

import "errors"

// ErrMySQLQueryLoweringMissing is why a MySQL package will not generate.
//
// The dialect seam has two implementations of the DECODE side (runtime/mydec,
// ADR-0007) and two of the DDL side (compile/myddl). It has ONE implementation
// of the query side: compile/pgsql serves both dialects. So a MySQL-dialect
// package came out carrying PostgreSQL SQL —
//
//	SELECT "id", "email" FROM "my_users"  ... WHERE "id" = $1  ... RETURNING "id"
//
// — and MySQL rejects it on the first statement. Measured against MySQL 8.4.11:
// Error 1064 on the double-quoted identifier (default sql_mode has no
// ANSI_QUOTES, so "my_users" is a string literal, not a table), Error 1064 on
// RETURNING, which MySQL 8 does not have, and $1 where MySQL wants ?.
//
// codegen.TestMySQLGeneratedPackageCompiles passed throughout, because
// compiling and executing are different claims. That test is the R9 gate and it
// is still worth having — it is what catches the decode seam rotting — but it
// was read as though it meant the dialect worked.
//
// Refusing is the storm-shaped answer to that. A generator that emits a package
// no server will accept has produced a silent wrong answer, and this codebase's
// rule is that silence is not an option: a construct the target cannot express
// is a generation error naming the target and the source line.
var ErrMySQLQueryLoweringMissing = errors.New(
	"codegen: the MySQL dialect is not wired to its query lowering yet, so a generated " +
		"package would carry PostgreSQL SQL — double-quoted identifiers, $1 placeholders, " +
		"and an insert output clause, none of which MySQL 8 accepts (Error 1064).\n" +
		"       What exists and is proven against MySQL 8.4.11:\n" +
		"         compile/myddl   the DDL\n" +
		"         compile/mysql   the query lowering — backtick identifiers, the bare ?,\n" +
		"                         and the JSON_TABLE list lowering from ADR-0010\n" +
		"         runtime/mydec   the decoders\n" +
		"         runtime.Placeholder  the carrier ADR-0010 decided\n" +
		"       What is missing: codegen still calls compile/pgsql for every statement it\n" +
		"       emits, whichever dialect was asked for, and the wire-level driver.\n" +
		"       See docs/PLAN.md M9. Generating anyway is available to storm's own seam\n" +
		"       test only, which asserts that the DECODE path compiles and nothing more.")

// allowUnexecutableMySQL lets storm's own R9 gate keep generating a MySQL
// package to compile it. Unexported on purpose: the property that test asserts
// is real and worth keeping, and it is not the property an adopter would think
// they were getting.
var allowUnexecutableMySQL bool

// AllowUnexecutableMySQLForTest re-enables MySQL generation for the seam test
// and restores the refusal when the returned function runs.
//
// Exported only to codegen's own external test package, which is why it says so
// in its name. If this is ever called from anywhere that is not a test in this
// repository, the refusal above has been defeated rather than lifted.
func AllowUnexecutableMySQLForTest() func() {
	allowUnexecutableMySQL = true
	return func() { allowUnexecutableMySQL = false }
}
