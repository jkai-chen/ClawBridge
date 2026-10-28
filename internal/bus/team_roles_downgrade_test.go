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

// noticeDowngradeSQLite returns a schema-45 database to schema 44's shape.
const noticeDowngradeSQLite = "DROP TABLE project_lead_notices;\nDROP TABLE project_lead_state;\n"

// noticeDowngradePostgres is noticeDowngradeSQLite for a Postgres schema.
func noticeDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".project_lead_notices;\nDROP TABLE " + schema + ".project_lead_state;\n"
}
