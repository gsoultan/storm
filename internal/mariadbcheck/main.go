// Command mariadbcheck emits a SQL script that PREPAREs and EXECUTEs what storm
// lowers for MariaDB, concentrating on the five constructs where it and MySQL
// part company.
package main

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/mariadb"
	"github.com/gsoultan/storm/compile/mysql"
)

func main() {
	cols := []string{"id", "email", "org_id"}
	fmt.Println("DROP TABLE IF EXISTS `mc_rows`;")
	fmt.Println("CREATE TABLE `mc_rows` (`id` BIGINT NOT NULL, `email` VARCHAR(320) NOT NULL," +
		" `org_id` BIGINT NOT NULL, PRIMARY KEY (`id`), KEY `ix_org` (`org_id`,`id`));")
	var vals []string
	for i := 1; i <= 300; i++ {
		vals = append(vals, fmt.Sprintf("(%d,'u%d@x.com',%d)", i, i, i%20))
	}
	fmt.Printf("INSERT INTO `mc_rows` VALUES %s;\n", strings.Join(vals, ","))
	fmt.Println("SET @s='u1@x.com'; SET @n=5; SET @j='[1,2]'; SET @newid=900001;")

	emit := func(label, sql, args string) {
		fmt.Printf("-- %s\nPREPARE `%s` FROM '%s';\n", label, label, strings.ReplaceAll(sql, "'", "''"))
		if args == "" {
			fmt.Printf("EXECUTE `%s`;\n", label)
		} else {
			fmt.Printf("EXECUTE `%s` USING %s;\n", label, args)
		}
		fmt.Printf("DEALLOCATE PREPARE `%s`;\n", label)
	}

	// The one MariaDB has and MySQL has not. Aliased so the gate can see it
	// came back rather than merely that the statement ran.
	ins, err := mariadb.InsertStmt("mc_rows", cols, []string{"id"})
	if err != nil {
		panic(err)
	}
	emit("mc_insert_returning", strings.Replace(ins, "RETURNING `id`", "RETURNING `id` AS `returned_id`", 1),
		"@newid, @s, @n")

	// The four MariaDB has not. Each must be the MariaDB spelling, not MySQL's.
	sel := mysql.SelectPrefix("mc_rows", cols)
	for i, m := range []int{mysql.LockShare, mysql.LockShareNoWait, mysql.LockShareSkipLocked} {
		emit(fmt.Sprintf("mc_share_lock_%d", i),
			sel+" WHERE `id` = ?"+mariadb.LockSuffix(m), "@n")
	}
	emit("mc_batch_window",
		strings.Replace(mariadb.TopNBatch("mc_rows", cols, "org_id", "BIGINT", ""),
			"\x00order\x00", " ORDER BY `id` DESC", 1), "@j, @n")

	// And the shared four fifths, so a divergence there is caught here too.
	emit("mc_select", sel+" WHERE `email` = ? ORDER BY `id` LIMIT ?", "@s, @n")
	emit("mc_count", mysql.CountPrefix("mc_rows")+" WHERE `org_id` = ?", "@n")

	// The recursive traversal. Shared with MySQL in shape, but run here too
	// because the failure it shipped with was not a syntax error: MariaDB
	// infers a recursive CTE column's type from the ANCHOR as well, so an
	// un-cast path column is one key wide and the first append overflows it.
	fmt.Println("DROP TABLE IF EXISTS `mc_nodes`;")
	fmt.Println("CREATE TABLE `mc_nodes` (`id` BIGINT NOT NULL, `parent_id` BIGINT NULL," +
		" PRIMARY KEY (`id`), KEY `ix_parent` (`parent_id`));")
	fmt.Println("INSERT INTO `mc_nodes` VALUES (1,NULL),(2,1),(3,2),(4,3),(9,10),(10,9);")
	fmt.Println("SET @roots='[1]';")
	for label, dir := range map[string]int{
		"mc_recursive_descend": mysql.Descend,
		"mc_recursive_ascend":  mysql.Ascend,
	} {
		emit(label, mysql.Recursive("mc_nodes", []string{"id", "parent_id"}, "id", "parent_id",
			"BIGINT", dir, ""), "@roots, @n")
	}
	// Labelled in the RESULT, not in a comment: the client strips comments, so
	// a check anchored on one reads an empty string and passes for the reason
	// it was written to catch.
	fmt.Printf("SELECT 'mc_recursive_levels' AS `probe`, COUNT(*) AS `n` FROM (%s) `_c`;\n",
		twoArgs(mysql.Recursive("mc_nodes", []string{"id", "parent_id"}, "id", "parent_id",
			"BIGINT", mysql.Descend, ""), "'[1]'", "10"))
	fmt.Printf("SELECT 'mc_recursive_cycle' AS `probe`, COUNT(*) AS `n` FROM (%s) `_c`;\n",
		twoArgs(mysql.Recursive("mc_nodes", []string{"id", "parent_id"}, "id", "parent_id",
			"BIGINT", mysql.Descend, ""), "'[9]'", "200"))
}

// twoArgs substitutes the two placeholders a traversal takes, so the statement
// can be run directly rather than prepared.
func twoArgs(sql, a, b string) string {
	return strings.Replace(strings.Replace(sql, "?", a, 1), "?", b, 1)
}
