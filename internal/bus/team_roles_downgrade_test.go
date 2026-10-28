package bus

// rolesDowngradeSQLite returns a schema-43 database to schema 42's shape.
const rolesDowngradeSQLite = "DROP TABLE team_role_mapping;\n"

// rolesDowngradePostgres is rolesDowngradeSQLite for a Postgres schema.
func rolesDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".team_role_mapping;\n"
}
