package bus

// kindDowngradeSQLite returns a schema-40 database to schema 38's shape:
// intake attribution (40) first, then the project kind (39).
const kindDowngradeSQLite = "DROP TABLE app_write_attributions;\nALTER TABLE intake_trusted DROP COLUMN principal;\nDROP TABLE project_kind_policy;\n"

// kindDowngradePostgres is kindDowngradeSQLite for a Postgres schema.
func kindDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".app_write_attributions;\nALTER TABLE " + schema + ".intake_trusted DROP COLUMN principal;\nDROP TABLE " + schema + ".project_kind_policy;\n"
}
