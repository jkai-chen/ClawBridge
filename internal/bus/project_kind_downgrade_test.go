package bus

// outboxDowngradeSQLite returns a schema-42 database to schema 41's shape.
const outboxDowngradeSQLite = "DROP TABLE linear_write_outbox;\n"

// kindDowngradeSQLite returns a schema-42 database to schema 38's shape:
// the Linear write outbox (42), issue subscribers (41), then intake attribution (40), then the
// project kind (39).
const kindDowngradeSQLite = outboxDowngradeSQLite + "DROP TABLE issue_subscriptions;\nDROP TABLE issue_subscribers_owner;\nDROP TABLE issue_subscribers_policy;\nDROP TABLE app_write_attributions;\nALTER TABLE intake_trusted DROP COLUMN principal;\nDROP TABLE project_kind_policy;\n"

// kindDowngradePostgres is kindDowngradeSQLite for a Postgres schema.
func kindDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".linear_write_outbox;\nDROP TABLE " + schema + ".issue_subscriptions;\nDROP TABLE " + schema + ".issue_subscribers_owner;\nDROP TABLE " + schema + ".issue_subscribers_policy;\nDROP TABLE " + schema + ".app_write_attributions;\nALTER TABLE " + schema + ".intake_trusted DROP COLUMN principal;\nDROP TABLE " + schema + ".project_kind_policy;\n"
}
