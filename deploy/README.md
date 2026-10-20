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

The pilot uses a source-only archive deployment; `/opt/clawbridge/repo` has no
remote Git credentials or `.git` directory. On the operator checkout, verify the
reviewed commit was pushed, then export exactly that commit (never the dirty tree):

```sh
DEPLOY_SHA=<full-reviewed-pushed-commit>
git cat-file -e "$DEPLOY_SHA^{commit}"
git archive --format=tar.gz --output=/tmp/clawbridge-release.tar.gz "$DEPLOY_SHA"
shasum -a 256 /tmp/clawbridge-release.tar.gz
# Transfer through the approved IAP path; compare SHA-256 on the VM before use.
gcloud compute scp /tmp/clawbridge-release.tar.gz \
  instance-claw-bridge-dev:/tmp/clawbridge-release.tar.gz \
  --project claw-bridge-dev --zone europe-west2-a --tunnel-through-iap
```

On the VM, first run `sudo systemctl start clawbridge-postgres-backup.service`
and require a successful result and uploaded checksum. Confirm the persistent
mount, existing containers, stable private env symlink, and sufficient disk space.
Extract into a new empty directory; retain the previous source directory under a
unique timestamped recovery name before moving the new directory to the stable
path. Do not replace `/opt/clawbridge/.env.phase3` or database/Caddy volumes.

```sh
# Set these again in the VM shell: local operator variables do not cross SSH.
DEPLOY_SHA=<full-reviewed-pushed-commit>
EXPECTED_ARCHIVE_SHA256=<checksum-recorded-on-operator-machine>
actual=$(sha256sum /tmp/clawbridge-release.tar.gz | cut -d ' ' -f1)
[ "$actual" = "$EXPECTED_ARCHIVE_SHA256" ] || exit 1
stamp=$(date -u +%Y%m%dT%H%M%SZ)
sudo mkdir "/opt/clawbridge/repo-next-$stamp"
sudo tar -xzf /tmp/clawbridge-release.tar.gz -C "/opt/clawbridge/repo-next-$stamp"
sudo mv /opt/clawbridge/repo "/opt/clawbridge/repo-recovery-$stamp"
sudo mv "/opt/clawbridge/repo-next-$stamp" /opt/clawbridge/repo
printf '%s\n' "$DEPLOY_SHA" | sudo tee /opt/clawbridge/repo/DEPLOYED_SHA
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml config --quiet
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml build api
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name clawbridge-phase3 \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d --no-deps api
```

Record the archive checksum, image ID, source fingerprints, recovery directory,
and deployed SHA. Check HTTPS `/healthz`, the exact four-field `/v1/config`
shape without printing credential values, unauthenticated execute 401, authenticated
read-only whoami and doctor. Run the backup service again and confirm publication.
Only recreate Caddy if its reviewed configuration changed and requires it.

For application-only rollback, preserve the failed release under another unique
name, restore the recorded recovery directory to `/opt/clawbridge/repo`, and
repeat Compose validation/build/API recreation above. Retain the same project,
private env and volumes; do not run `down -v`. The external `DEPLOYED_SHA` marker
is operator evidence, not a cryptographic attestation; compare archived source
fingerprints as well.

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

### Reviewed exact-SHA archive update — 2026-10-20 22:27 UTC

The approved pilot was updated from the pushed commit
`f913ffa80443a47bf0faa62b55936a14802c570e` using `git archive`, transferred via
IAP. No remote Git checkout or credentials were introduced. Archive SHA-256:
`95cbed36b52ef2e5f6e51385cfddf551ef9959e7e81733a58fb3fc9cbe0aea52`.
The checksum was compared on the VM before extraction. Previous sources remain
at `/opt/clawbridge/repo-recovery-20261020T222255Z`; the stable source path is
`/opt/clawbridge/repo`, with an external `DEPLOYED_SHA` marker. The private env,
persistent data and Compose project were unchanged.

The standard Dockerfile build completed on the VM. Running API image ID:
`sha256:211e529908ce9a46667e84923a36094e0286e61f4b093f6126e00b2c80e1bfb2`.
Inspection confirmed `nonroot:nonroot`, read-only root filesystem, and running
state. Only the API container was recreated; Caddy and both healthy databases
retained their running containers. The older Phase 2 API remained stopped.
The reviewed source SHA-256 fingerprints matched the transferred source:

| File | SHA-256 |
| --- | --- |
| `internal/setup/setup.go` | `37a6e0f423877b9d419d12d6c8242818da1e337e7c9fdd5c3d57177590a51e52` |
| `internal/tui/view.go` | `77f64b7920d7495e591050493ef0e8149c3bc41f242c1d32c6f5a919de47fcd5` |
| `internal/doctor/doctor.go` | `e7431788286589a34b64e8df716c34ec2353afbb96a9a52b72d7197c09013ee6` |
| `internal/cli/cli.go` | `34e02cfc2cdf2c35421524c12a25a689a8f76710e660c81c8b31bb4227b05b65` |

Focused engineer postdeploy checks against
`https://34-142-66-102.sslip.io` passed: HTTPS `/healthz` 200; `/v1/config` 200
with exactly the four nonempty string fields (values withheld); unauthenticated
`POST /v1/execute` 401; authenticated read-only `whoami` returned `source=server`,
cloud storage and verified Google identity for the existing explicit
service-account impersonation route. Doctor exited 0: trusted configuration,
authority health, token selection and immutable project resolution passed. Its
only warnings were no complete matching mirror and no selected agent MCP config
paths. Checks used a temporary unbound routing config; the old probe config's
nonexistent session was omitted without changing any installed configuration.
No new consent or OAuth/keychain acceptance was performed for this update; the
separate root live evidence and previously documented platform limits still apply.

The mounted data remained `/dev/sdb` at `/var/lib/clawbridge-postgres` and row
counts remained one workspace, one project, five sessions and four messages.
Pre-update backup `postgres/daily/20261020T222251Z.dump` and checksum uploaded
successfully (SHA-256
`d9506ed8404db653662542ba98030d7356a1a77bef4ca02609fd089b7a6f045c`).
Post-update backup `postgres/daily/20261020T222706Z.dump` and checksum uploaded
successfully (SHA-256
`8fbe75a054d37d86348a43341cdf5712963acc0273f46793e0f3347053fd4f2a`).
Both use the existing `clawbridge-backups-claw-bridge-dev` bucket. This update
reused the earlier isolated restore proof; it did not repeat a restore or reboot.

Capacity at deployment: the 9.7 GiB boot filesystem was 97% used with about
329 MiB free after the successful build. All existing images, volumes and recovery
sources were preserved; no pruning, disk expansion, new resources or IAM grants
were performed. Future builds need a deliberate capacity check; if native builds
cannot fit, use an exact-SHA locally built Linux image/binary preserving the
reviewed Dockerfile runtime guarantees rather than deleting recovery artifacts.

### Boot-disk cleanup — 2026-10-20 22:34 UTC

The owner authorized cleanup of the pilot boot disk. Engineer inventoried usage
and proposed inactive Docker build cache, downloaded APT packages and the verified
transfer archive. Reviewer independently approved these targets and captured the
retained-resource baseline. An Orca queued-question state blocked further prompts
to the engineer, so root executed the agreed operational cleanup directly.

Before cleanup, Docker reported 42 inactive build-cache records (4.955 GB),
APT archives occupied 206 MB, and `/` had only 329 MB free (97% used). Root
confirmed no active build processes and compared the transfer archive against
the recorded SHA-256 before running only:

```sh
sudo docker builder prune --all --force
sudo apt-get clean
sudo rm -- /tmp/clawbridge-f913ffa80443.tar.gz
```

Builder pruning reported 5.086 GB reclaimed. After cleanup, `/dev/sda1` was
38% used with 5.7 GB available; Docker build cache was zero and APT archives
were 16 KB. The data disk remained 1% used (49 MB). All five exact container IDs,
seven image IDs (including dangling rollback images), and four volume names
(including the anonymous volume) matched the baseline. No service was restarted.
Current and recovery sources, private environment, database/Caddy volumes,
backups and journals were preserved. The removed archive is reproducible from
the published commit; its earlier checksum evidence remains historical.

Root verified the deployed marker remains `f913ffa80443a47bf0faa62b55936a14802c570e`,
the persistent mount and four source/recovery directories remain present, and
the private environment remains mode 0600 with the stable symlink. Docker and
the backup timer remained active; the timer remained enabled, the last backup
service result was success and the installed backup runner remained executable.
Postgres row counts remained `1/1/5/4` (workspaces/projects/sessions/messages).
An authenticated read-only `whoami` through the existing explicit operator
impersonation route returned `source=server`, verified Google identity and the
expected pilot/project. This did not repeat end-user OAuth consent.
Independent post-cleanup verification is recorded in PHASE-3-REVIEW.md.

Check `df -h /` and `docker system df` before future builds. Inactive builder
cache and package downloads can be regenerated; remove them only when no build
is active. Do not substitute `docker system prune`, image pruning or volume
pruning: retained recovery images and database volumes may appear unused.
No scheduled cleanup, disk expansion or additional billed resource was added.

### Boot-disk disposable-cache cleanup — 2026-10-20 22:35 UTC

The owner authorized boot-disk inspection and cleanup after the reviewed deploy.
The initial read-only inventory showed `/dev/sda1` (9.7 GiB) at 97% used:
8.8 GiB used, 329 MiB available. The separate database disk remained 20 GiB with
49 MiB used. `docker system df` showed 42 inactive build-cache records with
4.955 GB reclaimable private cache (builder inventory including shared data:
5.086 GB); downloaded APT archives were 206 MiB (33 `.deb` files,
215,433,248 bytes). APT lists were 161 MiB, journals 24 MiB and container logs
about 240 KiB. These latter small/diagnostic caches were left alone.

The proposed exact targets were sent for reviewer inspection before cleanup:
`sudo docker builder prune --all --force` (build cache only),
`sudo apt-get clean` (downloaded package archives), and only the known transfer
copy `/tmp/clawbridge-f913ffa80443.tar.gz` after checksum comparison. No
system/image/volume prune was authorized or used. The archive checksum had been
verified as `95cbed36b52ef2e5f6e51385cfddf551ef9959e7e81733a58fb3fc9cbe0aea52`.
The uncertain `/tmp/clawbridge-postboot-ops.sh` and all other temp files were
left alone. Reviewer tool delivery disconnected; the exact candidate inventory
was also surfaced to the coordinator.

Engineer follow-up: the engineer cleanup invocation reported `Total: 0B` from
builder pruning, completed `apt-get clean`, then stopped safely at its checksum
precondition because the transfer archive was already absent. A subsequent
read-only inventory established that the build cache had already become empty
between initial inspection and that invocation. The engineer did not execute the
following archive `rm`. Root's earlier 22:34:14 UTC invocation, recorded in the
preceding section and independently reconciled by reviewer, accounts for the
reclaimed cache and removed archive. The engineer's later invocation was an
empty-cache recheck; do not attribute the 5.086 GB reclamation to its zero-byte
prune result. No further deletion was needed.

The final inventory showed 38% boot usage: 3.5 GiB used and 5.7 GiB available,
comfortably above the 20% free target. Build cache was zero and APT archives
16 KiB. All seven image IDs, five container IDs, four volume names, source and
recovery directory names, container start timestamps and restart counts compared
identically against the pre-cleanup baseline. Protected private-env and deployed
SHA checksums also matched (private checksums were not printed). Thus current
and rollback images, the stopped Phase 2 API, both databases, Caddy volumes,
recovery sources and credentials were preserved. No service restart, disk
expansion, new resources or IAM grants occurred.

Focused engineer post-cleanup verification passed: HTTPS health 200, exact four
nonempty config fields without printing values, unauthenticated execute 401,
and authenticated read-only whoami with `source=server` through the existing
explicit impersonation route. Database counts remained 1 workspace / 1 project /
5 sessions / 4 messages. Docker and the backup timer were active; the latest
backup service result remained successful, the runner executable and stable env
readable, with `CLAWBRIDGE_DEPLOY_DIR=/opt/clawbridge/repo`. The Docker mount-guard
drop-in remained installed and `/dev/sdb` remained mounted at
`/var/lib/clawbridge-postgres`. Backup readiness was checked without creating an
extra dump, restart, reboot or restore drill. Reviewer was notified for
independent read-only capacity/resource/HTTPS/readiness checks. No heavy tests,
new consent, staging or commit were performed for this maintenance.
