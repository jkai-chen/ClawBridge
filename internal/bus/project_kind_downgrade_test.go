package bus

// webhookDowngradeSQLite returns a webhookSchemaVersion database to the
// shape before it: the webhook deliveries, team states, counters and
// cursor hints. The chain that uses it sets the version.
const webhookDowngradeSQLite = "DROP INDEX webhook_deliveries_team;\nDROP TABLE webhook_deliveries;\nDROP TABLE webhook_teams;\nDROP TABLE webhook_counts;\nDROP TABLE webhook_server;\nALTER TABLE watch_cursors DROP COLUMN hinted_at;\n"

// webhookDowngradePostgres is webhookDowngradeSQLite for a Postgres schema.
func webhookDowngradePostgres(schema string) string {
	return "DROP INDEX " + schema + ".webhook_deliveries_team;\nDROP TABLE " + schema + ".webhook_deliveries;\nDROP TABLE " + schema + ".webhook_teams;\nDROP TABLE " + schema + ".webhook_counts;\nDROP TABLE " + schema + ".webhook_server;\nALTER TABLE " + schema + ".watch_cursors DROP COLUMN hinted_at;\n"
}

// budgetDowngradeSQLite returns a schema-50 database to schema 49's shape:
// the budget's split.
const budgetDowngradeSQLite = "ALTER TABLE watch_lease DROP COLUMN budget_agents;\nALTER TABLE watch_lease DROP COLUMN budget_people;\n"

// budgetDowngradePostgres is budgetDowngradeSQLite for a Postgres schema.
func budgetDowngradePostgres(schema string) string {
	return "ALTER TABLE " + schema + ".watch_lease DROP COLUMN budget_agents;\nALTER TABLE " + schema + ".watch_lease DROP COLUMN budget_people;\n"
}

// ledgerScopeDowngradeSQLite returns a schema-49 database to schema 48's
// shape: the shared operation ledger's project, principal,
// acknowledgement and pending columns.
const ledgerScopeDowngradeSQLite = "DROP INDEX linear_write_ledger_pending;\nALTER TABLE linear_write_ledger DROP COLUMN pending;\nALTER TABLE linear_write_ledger DROP COLUMN project_id;\nALTER TABLE linear_write_ledger DROP COLUMN principal;\nALTER TABLE linear_write_ledger DROP COLUMN acknowledged_at;\n"

// maintenanceDowngradeSQLite returns a schema-49 database to schema 47's
// shape: the ledger's scope (49); the server drain's tables (48) are
// hosted only, so nothing more in SQLite.
const maintenanceDowngradeSQLite = ledgerScopeDowngradeSQLite

// maintenanceDowngradePostgres returns a schema-49 Postgres schema to 47's
// shape: the ledger's scope (49), then the server drain's maintenance row
// and instance reports (48).
func maintenanceDowngradePostgres(schema string) string {
	return "DROP INDEX " + schema + ".linear_write_ledger_pending;\nALTER TABLE " + schema + ".linear_write_ledger DROP COLUMN pending;\nALTER TABLE " + schema + ".linear_write_ledger DROP COLUMN project_id;\nALTER TABLE " + schema + ".linear_write_ledger DROP COLUMN principal;\nALTER TABLE " + schema + ".linear_write_ledger DROP COLUMN acknowledged_at;\nDROP TABLE " + schema + ".server_maintenance_instances;\nDROP TABLE " + schema + ".server_maintenance;\n"
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
