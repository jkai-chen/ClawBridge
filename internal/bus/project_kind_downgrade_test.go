package bus

// kindDowngradeSQLite returns a schema-41 database to schema 38's shape:
// issue subscribers (41) first, then intake attribution (40), then the
// project kind (39).
const kindDowngradeSQLite = "DROP TABLE issue_subscriptions;\nDROP TABLE issue_subscribers_owner;\nDROP TABLE issue_subscribers_policy;\nDROP TABLE app_write_attributions;\nALTER TABLE intake_trusted DROP COLUMN principal;\nDROP TABLE project_kind_policy;\n"

// kindDowngradePostgres is kindDowngradeSQLite for a Postgres schema.
func kindDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".issue_subscriptions;\nDROP TABLE " + schema + ".issue_subscribers_owner;\nDROP TABLE " + schema + ".issue_subscribers_policy;\nDROP TABLE " + schema + ".app_write_attributions;\nALTER TABLE " + schema + ".intake_trusted DROP COLUMN principal;\nDROP TABLE " + schema + ".project_kind_policy;\n"
}
