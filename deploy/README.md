# Single-VM deployment and recovery

The live pilot exists; this repository does not provision or reconfigure it.
The application, Postgres and Caddy are designed to run on one Compute Engine VM,
and the live recovery resources are already provisioned. Only Caddy publishes a
host port, TCP 443. The API and Postgres use an internal-only backend network.
Caddy and the API also join a non-internal egress network for ACME, OCSP and
Google token-signing certificates. The distroless application uses a read-only
filesystem with no Linux capabilities and runs as a non-root user.

The live pilot runs on `instance-claw-bridge-dev`, a `g1-small` Debian VM in
`europe-west2-a`, with a 10 GiB boot disk and a reserved static external IP. The
database lives on a separately attached persistent disk mounted at
`/var/lib/clawbridge-postgres`. Postgres itself uses the `data/` subdirectory so
the ext4 root and its `lost+found` directory are never initialized as database
storage. The data does not live on the boot disk or in a boot-directory fallback.
The private Compose environment file is `/opt/clawbridge/.env.phase3`, exposed
through `/etc/clawbridge/clawbridge.env` as a stable symlink. The reviewed checkout
is `/opt/clawbridge/repo`; the backup service explicitly sets
`CLAWBRIDGE_DEPLOY_DIR=/opt/clawbridge/repo`. The private off-VM backup bucket is
selected in the private environment as `CLAWBRIDGE_BACKUP_BUCKET`.

The pilot has a daily Compute Engine disk snapshot schedule, a retained pre-Phase
3 disk snapshot, and the private off-VM bucket described below. This repository
defines the daily custom-format database runner and timer that install onto that
VM. A restore drill has already loaded a custom-format dump into a separate
disposable Postgres target and verified the schema and message rows. These
recovery facts do not claim that every uncommitted Phase 3 application change has
been deployed.

The running VM, disk and services remain one live failure domain; the off-VM
artifact and disk snapshots are the recovery boundary. The default project
network also has inherited ICMP, RDP and broad internal rules; the previous
world-open SSH rule was disabled. Review those inherited rules before treating
the pilot as a team service.

## Decisions before replacing or extending the pilot

For a replacement VM or additional environment, the owner still selects the GCP
project, zone, machine and disk size, static address, backup bucket,
retention, and Google identities. Estimate VM, persistent disk, snapshots,
off-VM storage, egress, public IPv4 or NAT, and registry costs before requesting
the explicit confirmation required by DESIGN.md. IAP access does not make these
resources free. No default gcloud project is selected by this documentation.

OS, Docker, database and Caddy patching, monitoring and recovery remain operator
work. Keep database data on the persistent disk and verify a restore from an
off-VM artifact; a disk attached to the same failed VM is not the recovery copy.

## Google identity

The reviewed Phase 2 pilot verifies Google-signed ID tokens for an explicit
audience and uses an explicitly selected gcloud account to impersonate a
controlled service account. The audited principal is the service account in the
token. Sharing one service account loses per-person attribution. No human email
is trusted from a header.

Phase 3 adds the preferred end-user path: browser OAuth with loopback and PKCE,
or device authorization on headless machines. The client stores only a refresh
credential in the native OS keychain and mints a short-lived, audience-bound ID
token. The server verifies signature, issuer, audience, expiry and verified email,
then independently enforces ClawBridge membership and project roles. Agents do
not receive the refresh credential or minted token. The user joining an existing
workspace therefore does not need gcloud installed.

The gcloud execution path and the explicitly configured authorized-user ADC plus
service-account impersonation path remain operator alternatives. An optional
per-human service account can preserve separate workload provenance, but the
verified human principal remains in audit records. Service accounts have no
native expiry: time-bound access is enforced with an IAM Condition on the human's
`roles/iam.serviceAccountTokenCreator` grant. Do not use downloaded service-account
key files in the client path.

Creating the ClawBridge backend and database still needs privilege to provision
the selected Google Cloud resources. Reusing either OAuth client or authorized-user
ADC as the resource-creation credential grants broader scope than data-plane
messaging, so it must require explicit setup and cost confirmation. IAP-signed HTTP
assertions are not implemented by this release.

## Prepare configuration manually

Set these in a private, access-restricted environment file outside version control:

```text
POSTGRES_PASSWORD=<random hexadecimal password>
CLAWBRIDGE_DOMAIN=clawbridge.example.com
CLAWBRIDGE_AUDIENCE=DESKTOP_CLIENT_ID.apps.googleusercontent.com,DEVICE_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_CLIENT_ID=DESKTOP_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=DEVICE_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_CLIENT_SECRET=DESKTOP_CLIENT_INSTALLED_APP_CREDENTIAL
CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET=DEVICE_CLIENT_INSTALLED_APP_CREDENTIAL
CLAWBRIDGE_BOOTSTRAP_OWNER=owner-identity@your-project.iam.gserviceaccount.com
CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data
CLAWBRIDGE_BACKUP_BUCKET=private-clawbridge-backup-bucket
COMPOSE_PROJECT_NAME=clawbridge-phase3
```

Use a random hexadecimal password so it can be interpolated safely into the
Postgres URL. Install the environment file as
`/etc/clawbridge/clawbridge.env`, owned by `root:root` with mode `0600`. This file
is a deployment secret, not a ClawBridge message. Docker configuration rendering
may include its value; keep rendered config and logs private.

The two OAuth client IDs must match the Desktop-app and TVs/Limited Input devices
clients in the Google project. Their matching Google installed-app application
credentials are supplied only through the two `*_CLIENT_SECRET` environment
entries shown above, never through the Compose command or any server flag. Both
IDs must appear exactly in `CLAWBRIDGE_AUDIENCE`; the server rejects an omitted
audience match, an unmatched credential or duplicate roles. Google treats native
application credentials as non-confidential after distribution, and the two
credentials are intentionally exposed with the IDs at the public `/v1/config`
discovery endpoint. They must still not appear in logs, error output,
MCP/installer arguments or agent configuration.

The Compose file retains a named-volume default so isolated configuration tests
can render without host paths. That default is not approved for production.
Production sets
`CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data` and installs
the Docker mount guard below. Changing the source of an initialized database
without copying or restoring its contents starts an empty database. The named
Caddy volumes preserve account keys, certificates and proxy configuration
across container recreation.

After approved VM setup, the operator can run Docker Compose with that environment
file and `deploy/compose.yaml`. This repository does not create the VM, firewall,
IAM bindings, disks or billing resources automatically. PostgreSQL should stay
private; do not publish its port to make a connection work.

Point the chosen DNS name at the VM's static external IP before starting Caddy.
Allow public ingress only on TCP 443. Do not publish API, Postgres, port 80 or the
Caddy admin port. Caddy obtains the initial certificate with TLS-ALPN over TCP
443 and `--disable-redirects` prevents an HTTP listener that cannot be reached.
Its separate egress network needs DNS, HTTPS and OCSP reachability; restrict egress
only if the selected firewall still permits those certificate and Google paths.
The VM needs outbound access to container registries, ACME, OCSP and Google's
token-signing certificate endpoints.

For an ingress firewall that allows only TCP 443, the operator must also permit
SSH/IAP through the separately reviewed administrative path; this repository does
not open SSH itself. TCP 443 exposes only Caddy. `/v1/config` is intentionally
public; `/v1/execute` continues to require a verified Google bearer token.
`/healthz` checks only the API process; Compose service health and external
monitoring must be configured separately. If DNS, TCP 443 or the egress path is
wrong, Caddy cannot obtain a certificate and HTTPS clients fail even if the API
container is running.

Keep the repository and environment file in a private deployment directory such as
`/opt/clawbridge/repo`. Rebuild and recreate the API after every application commit;
restarting a VM only restarts the existing container image.

## Install the persistent-disk and backup guards

These commands assume the reviewed checkout is `/opt/clawbridge/repo`, the initialized
data disk is already attached and mounted, and the VM's Google identity may create
objects in the private backup bucket. Do not format or initialize an existing data
disk while installing the guard.

First verify the existing filesystem and record its UUID. If the live disk uses
`/etc/fstab`, an equivalent line is required so a failed mount can leave the VM
reachable for repair:

```sh
sudo findmnt -T /var/lib/clawbridge-postgres
sudo blkid /dev/disk/by-id/DATA_DISK
sudo cp /etc/fstab /etc/fstab.clawbridge-pre-mount-guard
# Replace DATA_DISK_UUID with the value printed above; do not use mkfs here.
if ! sudo grep -Eq '[[:space:]]/var/lib/clawbridge-postgres[[:space:]]' /etc/fstab; then
  sudo sh -c 'printf "%s\n" "UUID=DATA_DISK_UUID /var/lib/clawbridge-postgres ext4 defaults,nofail 0 2" >> /etc/fstab'
fi
sudo systemctl daemon-reload
sudo mount -a
sudo mountpoint -q /var/lib/clawbridge-postgres
sudo mkdir -p /var/lib/clawbridge-postgres/data
sudo findmnt -T /var/lib/clawbridge-postgres/data
```

The `nofail` option permits an IAP/SSH recovery session when the disk is absent.
The Docker drop-in is therefore mandatory: it blocks the Docker daemon from
starting if the exact directory is not a mountpoint. This prevents Postgres from
silently creating a new database under the boot filesystem.

```sh
sudo install -d -o root -g root -m 0755 /etc/systemd/system/docker.service.d
sudo install -o root -g root -m 0644 \
  /opt/clawbridge/repo/deploy/systemd/docker.service.d/10-clawbridge-postgres-mount.conf \
  /etc/systemd/system/docker.service.d/10-clawbridge-postgres-mount.conf
sudo systemctl daemon-reload
sudo systemctl restart docker
sudo systemctl is-active docker
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d
```

At this point Docker must start only while `/var/lib/clawbridge-postgres` is
mounted. If it is absent after a reboot, attach or restore the disk, verify
`/etc/fstab`, run `sudo mount -a`, inspect `sudo systemctl status docker`, and
then start Docker. Never bypass the guard by starting the stack against the empty
boot-directory mountpoint.
The guard protects the whole Docker daemon on this host, so restarting Docker
affects every container on the VM; inspect `docker ps -a` before installing or
testing it.

The live VM keeps the private environment at
`/opt/clawbridge/.env.phase3` and exposes the stable systemd path as a symlink.
Verify the target and link it before installing the units:

```sh
sudo install -d -o root -g root -m 0755 /etc/clawbridge /usr/local/libexec/clawbridge
test -f /opt/clawbridge/.env.phase3
sudo ln -sfn /opt/clawbridge/.env.phase3 /etc/clawbridge/clawbridge.env
sudo test -r /etc/clawbridge/clawbridge.env
sudo install -o root -g root -m 0755 \
  /opt/clawbridge/repo/deploy/backup-postgres.sh \
  /usr/local/libexec/clawbridge/backup-postgres.sh
sudo install -o root -g root -m 0644 \
  /opt/clawbridge/repo/deploy/systemd/clawbridge-postgres-backup.service \
  /etc/systemd/system/clawbridge-postgres-backup.service
sudo install -o root -g root -m 0644 \
  /opt/clawbridge/repo/deploy/systemd/clawbridge-postgres-backup.timer \
  /etc/systemd/system/clawbridge-postgres-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now clawbridge-postgres-backup.timer
sudo systemctl start clawbridge-postgres-backup.service
sudo systemctl status clawbridge-postgres-backup.service --no-pager
sudo systemctl list-timers clawbridge-postgres-backup.timer --no-pager
```

The service runs as root because it needs the local Docker socket and the VM's
Google identity. The runner uses `umask 077`, a protected temporary directory and
a cleanup trap. It parses only the backup bucket, production data path and Compose
project name from the private environment file, passes that file to Compose, and
never sources it into the backup process. Docker, Postgres and gcloud read their
own credentials directly; the runner does not print the environment, password,
token, client secret or credential file contents.

## HTTPS and operator access

Configure ClawBridge's server URL as `https://CLAWBRIDGE_DOMAIN` and the same
ordered audience set on the server and client route. The four installed-app
discovery values are available at `https://CLAWBRIDGE_DOMAIN/v1/config`. Every
application request still needs its own verified Google ID token; DNS or TLS
access alone is not per-request identity.

Use an explicit administrative access path for shell access and disk maintenance.
The application no longer needs an IAP tunnel to its loopback API for normal
clients. The VM needs an approved egress path for image retrieval, ACME and
Google signing-key refresh. Include static-address, disk, egress and registry
implications in the cost estimate.

## Backup and restore

The database recovery copy is a Postgres custom-format archive, not a copy of the
attached block device. Each run produces a same-timestamp pair:

```text
gs://CLAWBRIDGE_BACKUP_BUCKET/postgres/daily/YYYYMMDDTHHMMSSZ.dump
gs://CLAWBRIDGE_BACKUP_BUCKET/postgres/daily/YYYYMMDDTHHMMSSZ.sha256
```

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
the dump followed by its checksum with `gcloud storage`. Keeping the checksum
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

If the bucket lifecycle already differs, reconcile it to retain no fewer than 30
daily pairs. The backup run's IAM needs object-create permission on this prefix;
the restore operator needs object-view permission. Do not grant bucket-wide delete
permission to the backup runner.

The separate Compute Engine disk snapshot schedule remains enabled. Inspect it
with the live policy and disk names rather than creating duplicates:

```sh
gcloud compute resource-policies list --filter='region:europe-west2'
gcloud compute resource-policies describe SNAPSHOT_POLICY \
  --region=europe-west2 --format=json
gcloud compute disks describe DATA_DISK_NAME \
  --zone=europe-west2-a --format='value(resourcePolicies)'
```

The pilot target is a daily disk snapshot with at least 14 days of retention and
`--on-source-disk-delete=keep-auto-snapshots`. The retained pre-Phase 3 snapshot
is deliberately exempt from routine cleanup and must remain until explicitly
retired by the owner. Disk snapshots provide volume-level rollback; the custom
dump provides an independently restorable logical copy. Neither replaces the
other.

### Restore drill

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

### Production restore

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

## Update and rollback

Before every update, run a backup and confirm its checksum and timer/service
result. Record the current commit, rendered service state and the most recent
verified backup timestamp. Never delete the only Postgres volume with
`docker compose down -v`.

Update only a clean, reviewed checkout:

```sh
cd /opt/clawbridge/repo
git status --short
PREVIOUS_SHA=$(git rev-parse HEAD)
git fetch origin main
git merge --ff-only origin/main
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml config --quiet
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml build api
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d
sudo systemctl start clawbridge-postgres-backup.service
```

Check `/healthz`, `/v1/config`, an authenticated read-only request, and the new
backup pair before declaring the update complete.

For an application-only rollback using the recorded revision:

```sh
cd /opt/clawbridge/repo
git checkout --detach "$PREVIOUS_SHA"
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml build api
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d --force-recreate api caddy
```

For a rollback that crosses a schema change, do not run the older binary against
the newer schema. Use the production restore procedure with a dump taken before
the migration. Compose project and Caddy volumes stay in place throughout; never
use `docker compose down -v`.

## Verified pilot state

On 20 October 2026, commit `67c8935` was built on the existing
`instance-claw-bridge-dev` VM. Real Google impersonation, token verification,
workspace/project creation, two-session message exchange, authorization denial,
snapshot publication and outage reads were exercised through an IAP-mediated
loopback tunnel. A container restart preserved the Postgres volume and message
state.

The live recovery controls now include the reserved static external IP, the
separate persistent data disk, the private off-VM bucket, scheduled daily disk
snapshots, the retained pre-Phase 3 snapshot, and a successful custom-format dump
restore into a disposable Postgres target. The frozen Phase 3 Caddy/public-OAuth server is now deployed on that VM.
HTTPS discovery and explicit operator-impersonation authenticated requests have
passed. Root subsequently completed actual browser/device Google consent,
fresh-process macOS Keychain refresh, whoami, doctor and logout acceptance; see
[PHASE-3-LIVE-ACCEPTANCE.md](../PHASE-3-LIVE-ACCEPTANCE.md). VM replacement,
team-scale operation, and live client model workflows remain unproven.

## Operational acceptance — 20 October 2026 UTC

This stage installed the existing backup runner, daily timer and Docker mount
precondition on the approved VM through IAP. It created no cloud resources or IAM
grants and preserved the Phase 2 recovery containers and volume. No application
implementation changes, commits or staging were part of this operational stage.
The deployment-path correction is confined to this runbook, the backup runner's
default checkout and the backup service's documentation/environment paths.

Installed paths and effective configuration:

- Checkout: `/opt/clawbridge/repo`.
- Private environment: `/opt/clawbridge/.env.phase3`; stable symlink:
  `/etc/clawbridge/clawbridge.env`.
- Runner: `/usr/local/libexec/clawbridge/backup-postgres.sh`.
- Service: `clawbridge-postgres-backup.service`, with explicit
  `CLAWBRIDGE_DEPLOY_DIR=/opt/clawbridge/repo`.
- Timer: `clawbridge-postgres-backup.timer`, enabled, daily 02:30 UTC,
  `Persistent=true`; next observed deadline was 21 October 2026 02:30 UTC.
- Docker drop-in:
  `/etc/systemd/system/docker.service.d/10-clawbridge-postgres-mount.conf`.
- Mounted disk: `/dev/sdb`, ext4, at `/var/lib/clawbridge-postgres`;
  production bind source: `/var/lib/clawbridge-postgres/data`.

Backup and isolated restore evidence:

| Evidence | Result |
| --- | --- |
| First uploaded archive | `gs://clawbridge-backups-claw-bridge-dev/postgres/daily/20261020T215100Z.dump` |
| Companion checksum | Same prefix, `20261020T215100Z.sha256` |
| SHA-256 | `a44fb43e05b7a26b1a14c17d6596ee582b438ee6d1f0cba692a6ac5ecf1d39ab` |
| Service result | `Result=success`, `ExecMainStatus=0` |
| Downloaded checksum verification | PASS |
| Restore target | Disposable `postgres:17-alpine`, `--network none`, no host data mount; removed after drill |
| Restored schema/counts | Schema version 1; workspaces/projects/sessions/messages = `1/1/4/3`, matching live |
| Post-reboot archive | Same bucket/prefix, `20261020T215427Z.dump` and `.sha256` |
| Post-reboot SHA-256 | `8589c11dc392072d3a3fea0c12da296ef8e03d33ec48c0646101fad338255c16` |
| Post-reboot service result | `Result=success`, `ExecMainStatus=0` |

The bucket previously deleted all objects after 30 days. Its lifecycle was
reconciled to the checked-in rule: delete only `postgres/daily/` objects aged
30 days. No other bucket settings or IAM grants were changed. Both VM boot and
data disks retain `default-schedule-1`: daily snapshots starting at 03:00 UTC,
14-day retention, `KEEP_AUTO_SNAPSHOTS`. The retained snapshot
`pre-phase3-instance-claw-bridge-dev-20261020` was observed READY and left intact.

One planned reboot was scheduled at 21:53:06 UTC. The old boot ID was
`8f015e19-09b2-4f5b-94e6-718e4f22530c`; the new boot ID is
`31f99cbc-5242-484d-a65f-8834c0cd2e98`, with boot time 21:53:24 UTC.
Docker became active after boot; its installed `ExecStartPre` is
`/usr/bin/mountpoint -q /var/lib/clawbridge-postgres`. The missing-mount predicate
was also exercised in a private mount namespace: unmounting only that namespace's
view made the predicate reject, while the real host mount remained intact. This
proves the guard predicate and mounted reboot recovery; it does not claim a
physical-disk-detachment boot test.

All existing container IDs and intended states survived:

| Container | ID prefix | Post-reboot state |
| --- | --- | --- |
| Phase 3 Caddy | `eec2e72dac26` | Running |
| Phase 3 API | `504b7591a0d8` | Running |
| Phase 3 Postgres | `8f108fb98a54` | Healthy; counts `1/1/4/3` |
| Phase 2 API | `da85795ede51` | Previously stopped, remains stopped |
| Phase 2 Postgres | `6ebfb5f0e01e` | Healthy; counts `1/1/4/3`; original `deploy_postgres-data` volume preserved |

Post-reboot public checks passed against
`https://34-142-66-102.sslip.io`: trusted HTTPS, `/healthz` 200, `/v1/config`
with exactly the four nonempty installed-app credential fields, unauthenticated
`/v1/execute` 401, and authenticated scoped `whoami` returning `source=server`.
Authentication used explicit operator service-account impersonation with a
short-lived Google ID token retained in memory; no token or installed-app
credential was printed. This is not browser/device user-consent evidence.

Local operational checks passed: `sh -n deploy/backup-postgres.sh`, focused
`TestDeploymentBackupRunnerFailsClosedWithoutConfiguration|TestCompose`, and
`git diff --check`. The timer was invoked manually before and after reboot; an
unattended 02:30 UTC firing and missed-schedule catch-up have not yet been observed.
Root subsequently completed browser/device consent and real macOS Keychain
acceptance; see [PHASE-3-LIVE-ACCEPTANCE.md](../PHASE-3-LIVE-ACCEPTANCE.md).
That acceptance intentionally added the now-retired session
`oauth-acceptance-20261020` and retained message
`msg_5222ccb2c509ebbe0083e2ef7bd8afe6`, bringing the Phase 3 pilot to one workspace,
one project, five sessions and four messages. The earlier `1/1/4/3` backup,
restore and reboot counts above remain the historical recovery baseline, not a
claim about state after acceptance. Physical disk-loss replacement remains a
separate recovery exercise.

## Verification and limits

Local real-Postgres verification is run with `integration/run-postgres.sh` and
documented in PHASE-2-REVIEW.md. Google signature verification is tested using
synthetic signed tokens and intercepted Google certificate responses; that does
not prove live IAM or real Google token minting. The dump restore drill proves the
logical backup path, while VM/disk replacement remains a separate recovery path.
Caddy HTTPS and the deployed four-field discovery endpoint have been exercised.
Actual browser/device Google consent and fresh-process macOS Keychain
refresh/whoami/doctor/logout are recorded separately in
[PHASE-3-LIVE-ACCEPTANCE.md](../PHASE-3-LIVE-ACCEPTANCE.md); the operator
impersonation probes above do not establish those end-user flows. Native
Windows/Linux keychains, unattended timer firing, physical disk-loss replacement
and live model-driven client execution remain unverified. Failure-injection
rollback is covered by automated fixtures, not a destructive live-Google drill.
