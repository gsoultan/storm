// A SEPARATE module on purpose.
//
// It measures a third-party driver, and storm's own go.mod must not gain a
// MySQL dependency for that — every adopter would carry it. A nested module is
// excluded from the parent's ./... patterns and dependency graph, so this
// builds only when run from inside this directory.
module github.com/gsoultan/storm/internal/mysqlspike

go 1.27

require github.com/go-sql-driver/mysql v1.9.4
