# Backup and recovery

Use the backup runner, systemd units and lifecycle template under `deploy/`.
The [deployment guide](deployment.md#persistent-disk-and-backup-guards) covers
the mount guard and unit installation. These commands operate on the selected
existing deployment; preserve its private environment and volume bindings.

## Coordination schema upgrade

The [coordination increment](../design/coordination.md) introduces schema version
2; [native Linear settings](../design/linear.md) add version 3,
[Linear project sets](../design/linear-migration.md) add version 4, and session
assignments add version 5. Before an
upgrade, stop API/client writers and save a verified recovery copy of the existing
version.

For PostgreSQL, use the dump/checksum pair and disposable restore drill below.
For local SQLite, use a consistent SQLite backup, keeping the original store
untouched; copying only the main file while WAL writes continue is insufficient.

The matching binary migrates on opening the store. Coordinate the client/service
upgrade: policy-aware clients need the new notification_status operation, and
older watchers do not honor recipient wake policy. An older binary refuses the
newer store. Native Linear board settings additionally require a service supporting
schema 3 and the taskboard operations. A failed migration must leave the prior
schema intact.

Rollback restores the pre-upgrade backup into a separate target and selects the
matching older binary/service. It does not downgrade the version marker in place.
Keep the upgraded store for reconciliation: writes accepted after the recovery
copy are not included in that copy. Source acceptance alone performs no deployment
or production migration.

The shared backup contains board UUIDs and mapping revisions, not personal Linear
credentials. Each teammate reconnects or restores their own native secure store
separately. A restored board mapping still requires that connection to verify
access to its teams/project; a display label cannot establish access.

### Session assignment upgrade

Schema 5 adds the `session_assignments` table and the nullable
`projects.coordinator_session` column; no existing row changes. The upgrade is
one-way: an older binary refuses the schema-5 store, so take and verify the
recovery copy above first, and keep the exact older image with it. Upgrade the
service before clients use the assignment operations; an older service answers
them with "unsupported operation". Older clients keep working and ignore the
`assignment` field that `list_sessions` rows gain.

### Linear project-set upgrade

L2 clients need the schema-4 service and versioned taskboard operations. Opening
an existing schema-3 store with the matching service adds `mapping_version` to
`taskboard_bindings`, defaulting existing rows to 1. Existing mapping JSON,
revisions, clear tombstones and other domain rows remain unchanged. A V2 read
normalizes an old mapping without rewriting it. Once a multi-project mapping is
saved, an older client's taskboard operations return `unsupported_mapping`
instead of choosing a project.

Before promoting schema 4, restore a fresh schema-3 recovery copy into a separate
target, open it with the candidate service, and compare all prior domain columns
and rows, including `taskboard_bindings`. Verify the new column defaults to 1
for prior rows, authorized V2 metadata reads work, and representable mappings
retain their V1 response. Keep a separate, verified schema-3 recovery target and
the exact older image for rollback. Replacing the image alone cannot roll back a
schema-4 database.

The upgrade changes shared ClawBridge settings; it does not connect to Linear,
create Linear projects or change issues. Each teammate's native connection stays
local. Source acceptance and a disposable migration test do not perform the
deployment or establish that a production backup was restored successfully.

## Backup format and schedule

The database recovery copy is a Postgres custom-format archive, not a copy of the
attached block device. Each run produces a same-timestamp pair:

```text
gs://CLAWBRIDGE_BACKUP_BUCKET/postgres/daily/YYYYMMDDTHHMMSSZ.dump
gs://CLAWBRIDGE_BACKUP_BUCKET/postgres/daily/YYYYMMDDTHHMMSSZ.sha256
```

The env file names the store in one of two ways, never both:

- `CLAWBRIDGE_BACKUP_URL`: `gs://BUCKET` (Google Cloud Storage) or `s3://BUCKET` (Amazon S3), a bucket name and
  nothing else. With `s3://`, `CLAWBRIDGE_BACKUP_REGION` names the bucket's AWS region, and the upload uses
  `aws s3 cp` with the instance's role (`deploy/aws-instance-role-policy.json`).
- `CLAWBRIDGE_BACKUP_BUCKET`: a Google Cloud Storage bucket, as before.

A missing, duplicated or malformed setting stops the runner before it dumps anything.

The runner executes:

```text
docker compose ... exec -T db pg_dump \
  --username=clawbridge \
  --dbname=clawbridge \
  --format=custom \
  --compress=6 \
  --no-owner \
  --no-acl
```

It validates that the result is a non-empty custom archive by asking the Postgres
container's `pg_restore --list` to read it. It then computes SHA-256 and uploads
the dump followed by its checksum with `gcloud storage` (or `aws s3 cp` for an `s3://` store). Keeping the checksum
upload last makes the presence of a complete pair the success marker. Temporary
files are mode-restricted and removed on success, error or signal. The runner
waits up to two minutes for `pg_isready` after boot, then fails without uploading
an empty or partial archive.

The systemd timer runs every day at **02:30 UTC**. `Persistent=true` runs a
missed interval after the VM returns, so an outage does not silently postpone
backup indefinitely. The service does not retry in a loop. A failed unit remains
failed with a nonzero status and must be investigated before the next scheduled
attempt.

Run and inspect one backup without printing deployment secrets:

```sh
sudo systemctl start clawbridge-postgres-backup.service
sudo systemctl status clawbridge-postgres-backup.service --no-pager
sudo journalctl -u clawbridge-postgres-backup.service --since today --no-pager
gcloud storage ls "gs://${CLAWBRIDGE_BACKUP_BUCKET}/postgres/daily/" --limit 10
```

Keep at least **30 complete daily dump/checksum pairs** off the VM. The checked-in
`deploy/gcs-lifecycle.json` deletes only objects under `postgres/daily/` after 30
days. Review the lifecycle before applying it:

```sh
gcloud storage buckets describe "gs://${CLAWBRIDGE_BACKUP_BUCKET}" --format=json
gcloud storage buckets update "gs://${CLAWBRIDGE_BACKUP_BUCKET}" \
  --lifecycle-file=/opt/clawbridge/repo/deploy/gcs-lifecycle.json
gcloud storage buckets describe "gs://${CLAWBRIDGE_BACKUP_BUCKET}" --format=json
```

On S3, `deploy/s3-lifecycle.json` does the same (30 days under `postgres/daily/`, and old versions after 7):
`aws s3api put-bucket-lifecycle-configuration --bucket BUCKET --lifecycle-configuration
file:///opt/clawbridge/repo/deploy/s3-lifecycle.json`.

If the bucket lifecycle already differs, reconcile it to retain no fewer than 30
daily pairs. The backup run's IAM needs object-create permission on this prefix;
the restore operator needs object-view permission. Do not grant bucket-wide delete
permission to the backup runner.

The separate Compute Engine disk snapshot schedule remains enabled. Inspect it
with the live policy and disk names rather than creating duplicates:

```sh
gcloud compute resource-policies list --filter='region:REGION'
gcloud compute resource-policies describe SNAPSHOT_POLICY \
  --region=REGION --format=json
gcloud compute disks describe DATA_DISK_NAME \
  --zone=ZONE --format='value(resourcePolicies)'
```

Use the selected snapshot policy and retention. Preserve any separately retained
recovery snapshot until the owner explicitly retires it. Disk snapshots provide volume-level rollback; the custom
dump provides an independently restorable logical copy. Neither replaces the
other.

## Restore drill

Run the drill against a disposable container with no network and no production
credentials. This proves that the object is complete and restorable without
overwriting the live database.

```sh
set -eu
umask 077
STAMP=YYYYMMDDTHHMMSSZ
DRILL_DIR=$(mktemp -d)
CONTAINER=clawbridge-restore-drill-$$
trap 'docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; rm -rf "$DRILL_DIR"' 0 1 2 3 15

gcloud storage cp \
  "gs://${CLAWBRIDGE_BACKUP_BUCKET}/postgres/daily/${STAMP}.dump" \
  "$DRILL_DIR/${STAMP}.dump"
gcloud storage cp \
  "gs://${CLAWBRIDGE_BACKUP_BUCKET}/postgres/daily/${STAMP}.sha256" \
  "$DRILL_DIR/${STAMP}.sha256"
(cd "$DRILL_DIR" && sha256sum -c "${STAMP}.sha256")

docker run -d --rm --name "$CONTAINER" --network none \
  -e POSTGRES_HOST_AUTH_METHOD=trust postgres:17-alpine
i=0
until docker exec "$CONTAINER" pg_isready -U postgres -d postgres; do
  i=$((i + 1))
  [ "$i" -lt 30 ] || exit 1
  sleep 1
done
docker exec -i "$CONTAINER" pg_restore \
  --username=postgres --dbname=postgres \
  --no-owner --no-acl --exit-on-error <"$DRILL_DIR/${STAMP}.dump"
docker exec "$CONTAINER" psql --username=postgres --dbname=postgres \
  --tuples-only --no-align --command \
  "SELECT (SELECT count(*) FROM workspaces), (SELECT count(*) FROM projects), (SELECT count(*) FROM sessions), (SELECT count(*) FROM messages);"
docker exec "$CONTAINER" psql --username=postgres --dbname=postgres \
  --command "SELECT version FROM clawbridge_schema;"
```

Record the date, object timestamp, checksum, schema version and row counts. A
custom-format restore into a disposable target and verification of the schema and
known message rows has already been completed for the pilot. Repeat the drill
after a Postgres major upgrade, a bucket/retention change, or a schema migration,
and at least quarterly. Delete the disposable container afterward; it has no
network or host volume.

## Production restore

1. Declare maintenance mode, stop `api` and `caddy`, and stop new client writes.
2. Select and checksum-verify one complete dump/checksum pair using the drill
   commands.
3. Attach a new empty data disk and mount it at a temporary path. Never restore
   over the only copy of the current data disk.
4. Point a disposable Compose project at that temporary path, start only `db`,
   and run `pg_restore` into the initialized `clawbridge` database with
   `--no-owner --no-acl --exit-on-error`.
5. Verify `clawbridge_schema`, table counts, an expected message ID and an
   authorized inbox read before exposing the API.
6. Move the restored filesystem to `/var/lib/clawbridge-postgres`, ensure the
   initialized database files are under its `data/` subdirectory, restore the
   production env path, start `db`, `api` and `caddy`, and run the client smoke
   and doctor checks.
7. Keep the old disk unchanged until the next verified backup and an agreed
   rollback window has elapsed.

Backing up Caddy's `/data` and `/config` volumes preserves account keys,
certificates and proxy configuration. It does not replace the Postgres dump.
