// Command myquerycheck emits a SQL script that PREPAREs and EXECUTEs every
// statement form compile/mysql lowers.
//
// It exists because a golden test proves the generator is consistent with
// itself and cannot prove the SQL is valid. That is what let the MySQL dialect
// emit PostgreSQL SQL through four releases while
// codegen.TestMySQLGeneratedPackageCompiles stayed green: compiling and
// executing are different claims.
//
// PREPARE rather than a driver, so storm gains no MySQL dependency for a check
// that is about SQL text — the same reasoning scripts/check/mysql.sh already
// applies to the DDL half. PREPARE is also the stronger assertion: it type-
// checks the statement against the real schema, placeholders included.
package main

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/mysql"
)

func main() {
	cols := []string{"id", "email", "name", "age"}
	sel := mysql.SelectPrefix("my_users", cols)
	pk := mysql.DefaultOrderBy([]string{"id"}, "id")

	eqA, eqB, _ := mysql.Frag("Eq", mysql.Ident("email"))
	gtA, gtB, _ := mysql.Frag("Gt", mysql.Ident("age"))
	likeA, likeB, _ := mysql.Frag("Like", mysql.Ident("name"))
	nullA, nullB, _ := mysql.Frag("IsNull", mysql.Ident("age"))
	inA, inB := mysql.InFrag(mysql.Ident("id"), "BIGINT", false)
	notInA, notInB := mysql.InFrag(mysql.Ident("id"), "BIGINT", true)
	setA, _ := mysql.SetFrag("name")

	type stmt struct {
		label string
		sql   string
		args  []string
	}
	stmts := []stmt{
		{"select_eq", sel + mysql.WhereLead + eqA + eqB + mysql.OrderSuffix(pk), []string{"@s", "@n"}},
		{"select_gt_desc", sel + mysql.WhereLead + gtA + gtB +
			mysql.OrderLead + mysql.OrderTerm(1, mysql.Ident("name")) + " LIMIT " + mysql.Placeholder,
			[]string{"@i", "@n"}},
		{"select_like", sel + mysql.WhereLead + likeA + likeB + mysql.OrderSuffix(pk), []string{"@s", "@n"}},
		{"select_isnull", sel + mysql.WhereLead + nullA + nullB + mysql.OrderSuffix(pk), []string{"@n"}},
		{"select_two_preds", sel + mysql.WhereLead + gtA + gtB + mysql.WhereSep + likeA + likeB +
			mysql.OrderSuffix(pk), []string{"@i", "@s", "@n"}},
		{"count", mysql.CountPrefix("my_users") + mysql.WhereLead + gtA + gtB, []string{"@i"}},
		{"exists", mysql.ExistsPrefix("my_users") + mysql.WhereLead + eqA + eqB + mysql.ExistsSuffix(), []string{"@s"}},
		{"limit_offset", sel + mysql.OrderLead + pk + mysql.LimitOffsetSuffix(true), []string{"@n", "@n"}},
		{"in_json_table", sel + mysql.WhereLead + inA + inB + mysql.OrderSuffix(pk), []string{"@j", "@n"}},
		{"not_in_json_table", sel + mysql.WhereLead + notInA + notInB + mysql.OrderSuffix(pk), []string{"@j", "@n"}},
		{"update", mysql.UpdatePrefix("my_users") + setA + mysql.WhereLead + eqA + eqB, []string{"@s", "@s"}},
		{"delete", mysql.DeletePrefix("my_users") + mysql.WhereLead + eqA + eqB, []string{"@s"}},
		// Keyset pagination. MySQL supports row comparison, so it crosses
		// unchanged; SQL Server does not and M10 will have to expand it.
		{"keyset_row_cmp", sel + mysql.WhereLead +
			mysql.TupleOpen + mysql.Ident("age") + mysql.TupleSep + mysql.Ident("id") + mysql.TupleClose +
			mysql.RowCmpOp(0) +
			mysql.TupleOpen + mysql.Placeholder + mysql.TupleSep + mysql.Placeholder + mysql.TupleClose +
			mysql.OrderLead + mysql.Ident("age") + mysql.OrderSep + mysql.Ident("id") + " LIMIT " + mysql.Placeholder,
			[]string{"@i", "@i", "@n"}},
	}
	// Every ordering direction the token stream can carry.
	for dir := 0; dir < mysql.NDirections; dir++ {
		stmts = append(stmts, stmt{
			fmt.Sprintf("order_dir_%d", dir),
			sel + mysql.OrderLead + mysql.OrderTerm(dir, mysql.Ident("age")) + " LIMIT " + mysql.Placeholder,
			[]string{"@n"},
		})
	}
	// Every lock mode. A mode is an index into the generated cache array, so
	// one that does not PREPARE would be a lock the caller silently never got.
	for m := 1; m < mysql.NumLockModes; m++ {
		stmts = append(stmts, stmt{
			fmt.Sprintf("lock_mode_%d", m),
			sel + mysql.WhereLead + eqA + eqB + " LIMIT " + mysql.Placeholder + mysql.LockSuffix(m),
			[]string{"@s", "@n"},
		})
	}
	// The insert, which has no output clause to carry.
	ins, err := mysql.InsertStmt("my_users", []string{"id", "email", "name", "age"}, nil)
	if err != nil {
		panic(err)
	}
	// A key no seeded row holds: the point is that the statement PREPAREs and
	// runs, and a duplicate would abort the whole script on line one of it.
	stmts = append(stmts, stmt{"insert", ins, []string{"@newid", "@s2", "@s", "@i"}})

	fmt.Println("DROP TABLE IF EXISTS `my_users`;")
	fmt.Println("CREATE TABLE `my_users` (`id` BIGINT NOT NULL, `email` VARCHAR(320) NOT NULL," +
		" `name` VARCHAR(120) NOT NULL, `age` SMALLINT, PRIMARY KEY (`id`), KEY `ix_name` (`name`));")
	// Enough rows that the optimiser has a choice to get RIGHT. With three
	// rows it drives from the table and calls that an index scan, which is
	// correct for three rows and proves nothing about the lowering: the claim
	// under test is that a list probe reaches the index instead of reading
	// every row, and that only becomes the cheaper plan once there are rows
	// worth skipping.
	var vals []string
	for i := 1; i <= 500; i++ {
		age := "NULL"
		if i%7 != 0 {
			age = fmt.Sprintf("%d", i%90)
		}
		vals = append(vals, fmt.Sprintf("(%d,'u%d@x.com','n%d',%s)", i, i, i, age))
	}
	fmt.Printf("INSERT INTO `my_users` VALUES %s;\n", strings.Join(vals, ","))
	fmt.Println("ANALYZE TABLE `my_users`;")
	fmt.Println("SET @s = 'u1@x.com'; SET @s2 = 'zz@x.com'; SET @n = 10; SET @i = 5; SET @j = '[1,2,3]'; SET @newid = 900001;")

	for _, s := range stmts {
		// A single-quoted SQL string inside PREPARE: the only thing that needs
		// escaping is the quote itself, and none of these carry one — but
		// doing it anyway is what keeps this honest if one ever does.
		fmt.Printf("-- %s\n", s.label)
		fmt.Printf("PREPARE `%s` FROM '%s';\n", s.label, strings.ReplaceAll(s.sql, "'", "''"))
		fmt.Printf("EXECUTE `%s` USING %s;\n", s.label, strings.Join(s.args, ", "))
		fmt.Printf("DEALLOCATE PREPARE `%s`;\n", s.label)
	}

	// ADR-0010's load-bearing claim: the JSON_TABLE form must still reach the
	// index. A lowering that is merely ACCEPTED but scans every row would have
	// traded a correctness problem for a performance one.
	fmt.Printf("-- in_uses_index\nEXPLAIN FORMAT=TREE %s;\n",
		strings.ReplaceAll(sel+mysql.WhereLead+inA+inB+" ORDER BY `id` LIMIT 10", "?", "'[1,2,3]'"))
}
