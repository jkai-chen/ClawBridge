package bus

// rolesDowngradeSQLite returns a schema-43 database to schema 42's shape.
const rolesDowngradeSQLite = "DROP TABLE team_role_mapping;\n"

// rolesDowngradePostgres is rolesDowngradeSQLite for a Postgres schema.
func rolesDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".team_role_mapping;\n"
}

// leadDowngradeSQLite returns a schema-44 database to schema 43's shape.
const leadDowngradeSQLite = "DROP TABLE project_lead_settings;\n"

// leadDowngradePostgres is leadDowngradeSQLite for a Postgres schema.
func leadDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".project_lead_settings;\n"
}
