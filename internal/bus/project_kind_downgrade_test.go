package bus

// maintenanceDowngradeSQLite returns a schema-48 database to schema 47's
// shape: the server drain's tables are hosted only, so nothing in SQLite.
const maintenanceDowngradeSQLite = ""

// maintenanceDowngradePostgres returns a schema-48 Postgres schema to 47's
// shape: the server drain's maintenance row and instance reports.
func maintenanceDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".server_maintenance_instances;\nDROP TABLE " + schema + ".server_maintenance;\n"
}

// ledgerDowngradeSQLite returns a schema-47 database to schema 46's shape:
// the Linear write reservations.
const ledgerDowngradeSQLite = "DROP TABLE linear_write_reservations;\n"

// ledgerDowngradePostgres is ledgerDowngradeSQLite for a Postgres schema.
func ledgerDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".linear_write_reservations;\n"
}

// lanesDowngradeSQLite returns a schema-46 database to schema 45's shape:
// the lane pins.
const lanesDowngradeSQLite = "DROP TABLE lane_pins_revision;\nDROP TABLE lane_pins;\n"

// lanesDowngradePostgres is lanesDowngradeSQLite for a Postgres schema.
func lanesDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".lane_pins_revision;\nDROP TABLE " + schema + ".lane_pins;\n"
}

// outboxDowngradeSQLite returns a schema-42 database to schema 41's shape.
const outboxDowngradeSQLite = "DROP TABLE linear_write_outbox;\n"

// kindDowngradeSQLite returns a schema-42 database to schema 38's shape:
// the Linear write outbox (42), issue subscribers (41), then intake attribution (40), then the
// project kind (39).
const kindDowngradeSQLite = outboxDowngradeSQLite + "DROP TABLE issue_subscriptions;\nDROP TABLE issue_subscribers_owner;\nDROP TABLE issue_subscribers_policy;\nDROP TABLE app_write_attributions;\nALTER TABLE intake_trusted DROP COLUMN principal;\nDROP TABLE project_kind_policy;\n"

// kindDowngradePostgres is kindDowngradeSQLite for a Postgres schema, with
// the server drain's tables.
func kindDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".linear_write_outbox;\nDROP TABLE " + schema + ".issue_subscriptions;\nDROP TABLE " + schema + ".issue_subscribers_owner;\nDROP TABLE " + schema + ".issue_subscribers_policy;\nDROP TABLE " + schema + ".app_write_attributions;\nALTER TABLE " + schema + ".intake_trusted DROP COLUMN principal;\nDROP TABLE " + schema + ".project_kind_policy;\n"
}
