# Hosted deployment

The checked-in deployment runs the API, PostgreSQL and Caddy with Docker Compose.
It joins existing cloud resources; the repository does not provision them.
Only Caddy exposes TCP 443. The API and database use an internal network; the API
and Caddy also need egress for Google signing certificates and TLS operations.

Choose the deployment project, VM/zone, DNS, persistent data disk, private
configuration, backup bucket and administrative access path for your environment.
Keep database data on a separately mounted disk and maintain an off-VM recovery
copy.

The private environment file is `/etc/clawbridge/clawbridge.env` (it may be a
symlink to a file kept elsewhere). Its `COMPOSE_PROJECT_NAME` names the Compose
project. Use the same `--project-name` everywhere, and never change it on an
existing deployment: Compose finds the running containers by it, and the Caddy
volumes (and the database volume, when `CLAWBRIDGE_POSTGRES_DATA_PATH` is not set)
are named after it. The commands below read it from a shell variable, so a
command copied without it stops instead of starting a second, empty stack:

```sh
PROJECT=$(sudo sed -n 's/^COMPOSE_PROJECT_NAME=//p' /etc/clawbridge/clawbridge.env)
```

This guide runs and updates a deployment. For a first deployment from nothing,
see the [first-deployment guide](first-deployment.md).
Historical pilot resource names and observations are in the
[deployment archive](../archive/reviews/deployment-pilot.md).

## Google identity

End users sign in with browser OAuth and loopback PKCE, or device authorization
on headless machines. The client stores only a refresh
credential in the native OS keychain and mints a short-lived, audience-bound ID
token. The server verifies signature, issuer, audience, expiry and verified email,
then independently enforces ClawBridge membership and project roles. Agents do
not receive the refresh credential or minted token. A project owner grants
access with `clawbridge project role-set EMAIL ROLE` and withdraws it with
`clawbridge project role-revoke EMAIL`: revocation retires that person's active
sessions in the project, and removes workspace membership once they hold no
project role. The workspace owner cannot be revoked, and owners cannot revoke
themselves. Joining an existing workspace
does not require gcloud.

### Account binding

The server binds each email principal to the Google account (the token's
`sub`) of its first request that passes the membership check, and refuses any
other account for that address with `forbidden`. Refusals are audited in
`principal_events` by a hash of the account, at most once per account and
kind each hour; a caller who is not a member is refused before binding and
writes nothing. When an address legitimately moves to a new Google account, the
server owner releases it:

```sh
clawbridge principal-unbind EMAIL --confirm
```

The next sign-in with that address binds its account, except the released one:
an old account may still hold a token valid for up to an hour, so it stays
refused. If the unbind was a mistake and the same account must bind again, run
the command again with `--allow-previous`. Only the most recently released
account stays refused: after a second unbind, the account released by the
first may bind again.

The server owner cannot release their own address with `principal-unbind`;
that would refuse the very account making the request. When the owner's own
address is the one that moved, the owner cannot sign in anyway. In both cases
the host operator runs the same unbind with direct database access; it is
audited as `operator:local`:

```sh
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
  -f /opt/clawbridge/repo/deploy/compose.yaml \
  run --rm --no-deps api operator principal-unbind EMAIL --confirm
```

### Workspace domain allow-list

A workspace owner can accept only accounts managed by named Google Workspace
domains (the token's `hd` claim):

```sh
clawbridge workspace-domains example.com corp.example --workspace TEAM
clawbridge workspace-domains --clear --workspace TEAM
```

The list holds at most 20 domains and must include the owner's own domain.
With a list set, a member whose account has another domain, or none (a
consumer Google account), is refused with `forbidden` and writes no binding.
Two exceptions:

- The workspace owner may always change or clear the list, even when the list
  no longer admits the owner's own account.
- A Google service account (any `…gserviceaccount.com` address: `iam.`,
  `developer.` or `appspot.`) never carries `hd`, so a service account that is
  an explicit workspace member is admitted. It still needs membership like any
  other principal.

A damaged list (null or unreadable) admits no one, service accounts included,
until the owner sets it again or the operator clears it. Only the workspace
owner sees the list (in `whoami`); other members and snapshots, and so offline
mirrors, never carry it. A session's heartbeat re-checks the list and the
membership before it records `last_seen`.

A refused member sees the generic `forbidden` (HTTP 403) for every command,
including the project lookup most commands start with. When a signed-in member
is refused with 403, the causes are: no workspace membership or project role,
an account that is not the one bound to the address, or a sign-in domain the
workspace's allow-list does not include. The workspace owner can always run
`workspace-domains`, so an owner whose domain changed repairs the list there.

If a list is damaged and admits no one, the host operator clears it:

```sh
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
  -f /opt/clawbridge/repo/deploy/compose.yaml \
  run --rm --no-deps api operator workspace-clear-domains TEAM --confirm
```

Run operator commands from the same release as the server. They never migrate
the database: they refuse a schema at any other version.

To check what the ClawBridge Linear app may do, the operator runs a probe as the
app:

```sh
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
  -f /opt/clawbridge/repo/deploy/compose.yaml \
  run --rm --no-deps api operator linear-app-probe --confirm [--project UUID]
```

- **What it reads:** the MCP tools Linear offers the app, and, with
  `--project`, which health fields that project has.
- **What it sends:** one project update for a random project that does not
  exist, so Linear's answer says whether the app's scopes allow project updates.
  Nothing is created. If Linear ever reports one was, the probe says so at once
  and prints that update's ID, the only ID it ever prints, so the owner can
  delete exactly that update.
- **Reading the answer:** only class `scope`, with an error type that names the
  scope or `write`, is conclusive: the app may not post project updates.
  `not_found` and `validation` suggest the scope check passed, but Linear may
  check scopes after looking up the project, so they and every other class are
  provisional.
- **What it prints:** classes, tool and field names, and value kinds. It never
  prints a token, secret, ID, error message or field value.
- **Tokens:** it uses only the token the server published, and never mints one.
  With none, or one close to expiry, it stops at `mint_disabled`: let the server
  refresh it first.

To settle whether the app may post project health, `--p8` posts **one real**
project status update as the app and removes it again:

```sh
... run --rm --no-deps api operator linear-app-probe --confirm --p8 --project UUID --dry-run   # checks only
... run --rm --no-deps api operator linear-app-probe --confirm --p8 --project UUID
```

- **The project** must be an empty throwaway: no issues (archived ones
  included), no members and no lead. Anything else is refused before a write,
  so nobody is notified.
- **Before sending,** it checks that the app's MCP tools can create, list and
  delete a project status update (`save_status_update`, `get_status_updates`,
  `delete_status_update`), with `project` as the update's type and `onTrack` as
  its health. Otherwise nothing is sent. `--dry-run` stops there and prints the
  three tools' fields and allowed values.
- **The run:** one update with health `onTrack` and the body "ClawBridge probe,
  will be deleted"; a read-back; its deletion; and a second read that must no
  longer list it. An answer lost on the save is found by its body and deleted
  all the same.
- **What it prints:** classes, the tools' fields, and the update's ID, nothing
  else. If it cannot prove the update deleted, it ends with an `ALERT` naming
  the ID (or saying to check the project), and the owner deletes it.
- It never mints; the token rules are as above.

Before the server posts health for a project, `--health-order` settles that the
health judgement's read holds that project's newest update, the one it acts on:

```sh
... run --rm --no-deps api operator linear-app-probe --confirm --health-order --project UUID
```

- **The reads:** the judgement's own project read, unchanged (`first: 5`,
  ordered by `createdAt`), and the same read with `last: 5`. Two reads as the
  app, no write.
- **What it prints:** each read's update IDs and creation times in the order
  Linear returned them, and each read's order (`newest_first`, `oldest_first`,
  `same_time`, `mixed` or `single`); `distinct`, how many different updates
  the two reads saw; `window_has_newest`, whether the judgement's read holds
  the newest of them; `conclusive`, true with more than five distinct updates;
  and `agree`, both. Never an update's body or author.
- **Reading the answer:** only `agree: true` (conclusive, and the read holds the
  newest) lets a project's post setting go on. With five updates or fewer both
  reads return them all and prove nothing: run it on a project with more.
  `window_has_newest: false` means the judgement could miss the latest update:
  keep post off and tell the coordinator.
- It never mints; the token rules are as above.

To check whether the app may list the workspace's Linear documents (which the
server watcher's document notices need), run `operator linear-app-doc-probe
--confirm` the same way.
- **What it does:** one read, `list_documents` for the last 30 days, with no
  content or titles, and nothing sent or created.
- **What it prints:** how many documents came back, whether the page was
  complete, how many there are by parent kind (team, project, initiative,
  issue, none), and whether the list is newest first. It never prints an ID,
  title or time, and it never mints a token.

Explicit gcloud or authorized-user ADC with service-account impersonation are
operator alternatives. Their audited principal is the service account in the
verified token; sharing that account shares its application identity. Configure
the initiating account, target account and audience explicitly.

Configure workspace membership and project permissions separately from Google
sign-in. Provisioning the VM, database, OAuth clients and IAM grants is an operator
workflow outside client onboarding. The service validates Google ID tokens;
IAP-signed HTTP assertions are outside its implemented identity path.

## Sign-in methods

The server accepts Google ID tokens, ClawBridge device keys, or both. The
mode is `CLAWBRIDGE_AUTH` in the environment file, passed to `serve --auth`:

| `CLAWBRIDGE_AUTH` | Accepts | Needs |
| --- | --- | --- |
| unset or `google` | Google sign-in only | `CLAWBRIDGE_AUDIENCE` and all four OAuth settings |
| `google,device` | Google and device keys | the Google settings, and `CLAWBRIDGE_DOMAIN` |
| `device` | device keys only | `CLAWBRIDGE_DOMAIN`; no Google setting |

- **An existing deployment keeps Google sign-in unchanged.** The default is
  `google` when `CLAWBRIDGE_AUTH` is unset. The Google settings may now be empty in
  Compose, but whenever Google sign-in is on, `serve` refuses to start unless the
  audience and all four OAuth settings are present. A Google deployment missing one
  stops; it never falls back to device keys.
- **Device keys sign requests against the public origin**, `https://` plus
  `CLAWBRIDGE_DOMAIN`. The origin is configuration, never read from `Host` or
  `X-Forwarded-*`. Changing the domain invalidates nothing stored, but every client
  must then use the new origin.
- `GET /v1/config` shows the effective mode as `auth_methods` and the origin as
  `public_origin`.
- A device's key is cached for 30 seconds per server process. The database re-checks
  the device in every request, so a revocation takes effect at once on every
  replica.
- **Rate limits** (in memory, per process): `POST /v1/join` allows 10 attempts an
  hour per client address and 5 per email; failed sign-ins are limited to 60 a minute
  per address. A refused sign-in is logged at most once an hour per address and
  kind, never with the request's credentials.

### Letting the owner in without Google: `operator bootstrap-invite`

On a device-only deployment no one can sign in until the owner has a device. The
host operator lets the bootstrap owner in with a one-time code. The same command
recovers an owner who has lost every device.

```sh
sudo docker compose --env-file /etc/clawbridge/clawbridge.env --project-name "$PROJECT" \
  -f /opt/clawbridge/repo/deploy/compose.yaml \
  run --rm --no-deps api operator bootstrap-invite --workspace WORKSPACE
```

- It creates `WORKSPACE`, owned by `CLAWBRIDGE_BOOTSTRAP_OWNER`, if it doesn't
  exist. For an existing workspace it refuses unless the bootstrap owner owns it.
- It prints a single-use code, valid for 24 hours, for the owner's email only. The
  owner then runs, on their own computer:
  `clawbridge join --server https://DOMAIN --email OWNER --code CODE`.
  (`clawbridge join` ships with the client in the same release.)
- **The code is a secret.** Run the command in your own terminal session, not
  through an agent, and type the code yourself. The command refuses to print to
  anything but a terminal unless given `--force-stdout`, which prints the code alone
  on one line for a program that keeps it in memory (`clawbridge cloud bootstrap`).
  The code is never logged; the audit records the invite, not the code.
- Accepting an ownership transfer revokes any pending bootstrap code.

## Private configuration

Set these in a private, access-restricted environment file outside version control:

```text
POSTGRES_PASSWORD=<random hexadecimal password>
CLAWBRIDGE_DOMAIN=clawbridge.example.com
CLAWBRIDGE_AUDIENCE=DESKTOP_CLIENT_ID.apps.googleusercontent.com,DEVICE_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_CLIENT_ID=DESKTOP_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=DEVICE_CLIENT_ID.apps.googleusercontent.com
CLAWBRIDGE_OAUTH_CLIENT_SECRET=DESKTOP_CLIENT_INSTALLED_APP_CREDENTIAL
CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET=DEVICE_CLIENT_INSTALLED_APP_CREDENTIAL
CLAWBRIDGE_BOOTSTRAP_OWNER=you@example.com
# Optional: google (the default), device, or google,device; see "Sign-in methods".
# CLAWBRIDGE_AUTH=google
CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data
CLAWBRIDGE_BACKUP_BUCKET=private-clawbridge-backup-bucket
COMPOSE_PROJECT_NAME=clawbridge
```

Use a random hexadecimal password so it can be interpolated safely into the
Postgres URL; `openssl rand -hex 32` makes one. `CLAWBRIDGE_BOOTSTRAP_OWNER` is
the verified identity that will run `clawbridge setup --cloud` once and become
the workspace owner: normally your own Google account's email, the one you sign
in with. Install the environment file as
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

### The ClawBridge Linear app (optional)

With these three entries, agents' Linear writes are made by the server as the
ClawBridge Linear app, never by a client. Creating that app in Linear (an OAuth
application with client credentials and the scopes below), installing it in the
Linear workspace and adding it to private teams are not yet documented step by
step here; they need a Linear workspace admin. Its redirect URL is
`https://CLAWBRIDGE_DOMAIN/linear/callback`, a static page that only says the app
is installed.

```text
CLAWBRIDGE_LINEAR_APP_CLIENT_ID=<the app's client ID>
CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET=<the app's client secret>
CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=<32 random bytes, base64>
```

- **The token key** is 32 random bytes in standard, padded base64 (44
  characters); `openssl rand -base64 32` makes one. Hex is refused.
- **All three or none.** The Compose file passes them to the API container and
  defaults each one to empty. With all three empty the server starts without
  app writes; with only some set it refuses to start.
- **No disk.** App writes keep their operation records in Postgres, so the
  read-only API container needs no extra mount.

- **Where they come from.** Only this environment file; never a flag, the
  database or a log. The startup log says only `linear app: configured`.
- **The token.** The server mints the app's client-credentials token with a fixed
  scope set (`read, write, comments:create, issues:create, app:assignable,
  app:mentionable`). It seals the token with AES-GCM under the token key and
  keeps it in Postgres, so every server instance shares one token.
  - `write` is optional. If Linear grants the rest without it, the server uses
    that token for everything except what needs `write` (issue relations and
    project status updates), and asks for `write` again once a week. Any other answered set is refused, counted
    and backed off.
- **Rotation.**
  - To rotate the secret, change it here and restart: every token is revoked,
    and the server mints a new one on its next Linear call.
  - Changing the token key makes the stored token unreadable, so the server mints
    another. Mints are counted and limited (50 in 30 days), and paced.
- **Workspace and teams.**
  - The app must be a member of every bound private team.
  - Each binding must be pinned to the app's Linear workspace: link it again once
    to pin it.
  - Writes to an unpinned binding refuse with `workspace_unpinned`.
- **Timeouts.** A write the server makes can take up to a minute, and it
  finishes even if the client disconnects. Keep the proxy and load-balancer
  timeouts for `/v1/execute` at 75 seconds or more.
- **Removing them.** Removing these entries from a server that offered app
  writes turns hosted agent writes off **by design**. They are refused
  (`app_unavailable`) rather than made with someone's personal account.
- **Who may write.** With app writes, a member's ClawBridge role alone
  authorises their Linear writes. To offboard someone, revoke their role.

### The server watcher (optional, v0.3)

With the Linear app configured, the server can run the comment watch, claim
markers and issue intake itself, for every bound project:

```text
CLAWBRIDGE_WATCH_INTERVAL=15m
```

- **The clock.** The API ticks the watcher in process at this interval, from 1m
  to 30m. Anything else refuses to start. Empty or unset means no watcher. Ticks
  never overlap, and each runs within 50 seconds. The startup log says `watcher:
  ticking every 15m0s`, or `watcher: off (…)` with the reason. Without the Linear
  app, the watcher stays off and the server serves as before.
- **Calls.** Each project's watch spends at most 200 Linear calls an hour on the
  bus budget. The interval sets latency more than cost: each issue and intake
  source keeps its own polling schedule, and a tick with nothing due calls
  nothing.
- **Clients.** While the server's ticks for a project are fresh, a client
  `clawbridge linear watch` is refused. Stop client watchers before turning this
  on. A client may take over again once the server has not ticked for 10
  minutes.
- **Intake** follows each project's intake policy (docs/linear.md). Set
  `clawbridge intake policy set --intake-mode off` first on any project that
  should not run it.
- **To stop it,** remove the entry and recreate the API container.
- The scheduler variables `CLAWBRIDGE_TICK_AUDIENCE` and
  `CLAWBRIDGE_TICK_SERVICE_ACCOUNT` serve a scheduler-driven tick endpoint, meant
  for a deployment without a long-lived process. Leave them unset on a VM.

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

## Persistent disk and backup guards

These commands assume the reviewed checkout is `/opt/clawbridge/repo`, the initialized
data disk is already attached and mounted, and the VM's Google identity may create
objects in the private backup bucket. Do not format or initialize an existing data
disk while installing the guard.

First verify the existing filesystem and record its UUID. If the selected data disk uses
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
  --project-name "${PROJECT:?set PROJECT first}" \
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

A deployment may keep the private environment elsewhere, for example at
`/opt/clawbridge/clawbridge.env`, and expose the stable systemd path as a symlink.
A deployment that installs the file directly at `/etc/clawbridge/clawbridge.env`
skips the `test -f` and `ln` lines below. Otherwise, verify the target and link it
before installing the units:

```sh
sudo install -d -o root -g root -m 0755 /etc/clawbridge /usr/local/libexec/clawbridge
test -f /opt/clawbridge/clawbridge.env
sudo ln -sfn /opt/clawbridge/clawbridge.env /etc/clawbridge/clawbridge.env
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

Configure ClawBridge's server URL as `https://CLAWBRIDGE_DOMAIN` and a server audience set containing the
client route's selected audience. The four installed-app
discovery values are available at `https://CLAWBRIDGE_DOMAIN/v1/config`. Every
application request still needs its own verified Google ID token; DNS or TLS
access alone is not per-request identity.

Use an explicit administrative access path for shell access and disk maintenance.
The application no longer needs an IAP tunnel to its loopback API for normal
clients. The VM needs an approved egress path for image retrieval, ACME and
Google signing-key refresh. Include static-address, disk, egress and registry
implications in the cost estimate.

## Update and rollback

**v0.2.5 (schema 7): upgrade the server before the clients.** A v0.2.5 server
refuses every Linear write from an older client ("upgrade ClawBridge (v0.2.5 or
later)"), and a v0.2.5 client gets "unsupported argument" from an older server.
Schema 7 is one-way: an older binary refuses the upgraded database, so roll back
only by restoring the pre-update backup. Claim markers post session names into
Linear, where every member of the team can read them; name sessions by role.

Before every update, run a backup and confirm its checksum and timer/service
result. Record the current commit, rendered service state and the most recent
verified backup timestamp. Never delete the only Postgres volume with
`docker compose down -v`.

For a source-only archive deployment, `/opt/clawbridge/repo` has no
remote Git credentials or `.git` directory. On the operator checkout, verify the
reviewed commit was pushed, then export exactly that commit (never the dirty tree):

```sh
DEPLOY_SHA=<full-reviewed-pushed-commit>
git cat-file -e "$DEPLOY_SHA^{commit}"
git archive --format=tar.gz --output=/tmp/clawbridge-release.tar.gz "$DEPLOY_SHA"
shasum -a 256 /tmp/clawbridge-release.tar.gz
# Transfer through the approved IAP path; compare SHA-256 on the VM before use.
gcloud compute scp /tmp/clawbridge-release.tar.gz \
  VM_NAME:/tmp/clawbridge-release.tar.gz \
  --project PROJECT_ID --zone ZONE --tunnel-through-iap
```

On the VM, first run `sudo systemctl start clawbridge-postgres-backup.service`
and require a successful result and uploaded checksum. Confirm the persistent
mount, existing containers, stable private env symlink, and sufficient disk space.
Extract into a new empty directory; retain the previous source directory under a
unique timestamped recovery name before moving the new directory to the stable
path. Do not replace the private environment file or the database/Caddy volumes.

```sh
# Set these again in the VM shell: local operator variables do not cross SSH.
DEPLOY_SHA=<full-reviewed-pushed-commit>
EXPECTED_ARCHIVE_SHA256=<checksum-recorded-on-operator-machine>
# Before the swap, so no command below stops half-way in a fresh shell.
PROJECT=$(sudo sed -n 's/^COMPOSE_PROJECT_NAME=//p' /etc/clawbridge/clawbridge.env)
[ -n "$PROJECT" ] || exit 1
actual=$(sha256sum /tmp/clawbridge-release.tar.gz | cut -d ' ' -f1)
[ "$actual" = "$EXPECTED_ARCHIVE_SHA256" ] || exit 1
stamp=$(date -u +%Y%m%dT%H%M%SZ)
sudo mkdir "/opt/clawbridge/repo-next-$stamp"
sudo tar -xzf /tmp/clawbridge-release.tar.gz -C "/opt/clawbridge/repo-next-$stamp"
sudo mv /opt/clawbridge/repo "/opt/clawbridge/repo-recovery-$stamp"
sudo mv "/opt/clawbridge/repo-next-$stamp" /opt/clawbridge/repo
printf '%s\n' "$DEPLOY_SHA" | sudo tee /opt/clawbridge/repo/DEPLOYED_SHA
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
  -f /opt/clawbridge/repo/deploy/compose.yaml config --quiet
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
  -f /opt/clawbridge/repo/deploy/compose.yaml build api
sudo docker compose --env-file /etc/clawbridge/clawbridge.env \
  --project-name "${PROJECT:?set PROJECT first}" \
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

See [backup and recovery](backup-and-recovery.md) for dump verification and restore.
