package bus

// kindDowngradeSQLite returns a schema-39 database to schema 38's shape.
const kindDowngradeSQLite = "DROP TABLE project_kind_policy;\n"

// kindDowngradePostgres is kindDowngradeSQLite for a Postgres schema.
func kindDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".project_kind_policy;\n"
}
