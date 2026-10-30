# Running the server on your own machine

This guide runs the ClawBridge server on a computer you already have, with Docker
Compose: your laptop, a desktop in the office, or a small box under a desk. It needs
no cloud account, no domain and no Google sign-in. People sign in with device keys
and invites ([Signing in](../sign-in.md)).

It's for:
- **one person** who wants the hosted server's features (several machines' agents,
  the TUI, the Linear app) without renting a VM;
- **a small team on one network** (an office, a home lab) that would rather keep the
  server on a machine it owns.

If your team works from different places, or needs the server up while this machine
sleeps, use a cloud server instead: [First deployment](first-deployment.md).

## The two shapes

| | Shape A, this machine only (`solo`, the default) | Shape B, your local network (`lan`) |
|---|---|---|
| Address | `http://127.0.0.1:8443` | `https://HOST`, such as `https://clawbridge.lan` |
| Who can reach it | programs on this machine | computers on your network, once you choose a LAN address |
| TLS | none: plain http, on loopback only | Caddy, with its own CA (B1, the default), mkcert (B2) or `tailscale cert` (B3) |
| Setup | one command | one command, then trust the certificate on each computer |

Both run the same three-container stack as a cloud server ([`deploy/compose.yaml`](../../deploy/compose.yaml):
Postgres, the API, and Caddy in shape B), with [`deploy/compose.local.yaml`](../../deploy/compose.local.yaml)
on top and [`deploy/local-host.sh`](../../deploy/local-host.sh) to drive it. The image is built from your
checkout.

## Prerequisites

You need a ClawBridge checkout (`git clone`), the `clawbridge` client for the people who sign in
([Installation](../installation.md)), `openssl`, and `curl` or `wget`. Then Docker with Compose 2.24.4 or newer
(`docker compose version`; the override file uses Compose's `!override` tag), which depends on the OS:
- **macOS:** OrbStack or Docker Desktop.
- **Linux:** Docker Engine 28 or newer, with the Compose plugin (`docker compose version` answers). Older
  engines let other machines on the same network segment reach ports published on `127.0.0.1`, which both
  shapes use. Run the script as your own user, in the `docker` group. Under `sudo`, HOME becomes `/root`, so
  the env file and backups would land in `/root/.clawbridge-server`; if you must use `sudo`, set
  `CLAWBRIDGE_LOCAL_DIR` explicitly (`sudo CLAWBRIDGE_LOCAL_DIR=/home/YOU/.clawbridge-server deploy/local-host.sh ...`).
- **Windows:** WSL2, with Docker Desktop's WSL backend or Docker Engine installed inside WSL. Run the
  script **inside WSL**, from a checkout in the WSL file system (`~/src/ClawBridge`, not `/mnt/c/...`).

The script checks Docker, the Compose version and the daemon before it does anything, and says which is missing
or too old.

Everything it keeps lives in one private folder, `~/.clawbridge-server`, made with mode 0700. Set
`CLAWBRIDGE_LOCAL_DIR` to put it elsewhere. The script refuses a folder that group or others can enter or write
(it holds the keys), and names the fix: `chmod 700 DIR`.

## Shape A: this machine only

```sh
deploy/local-host.sh up --owner owner@example.test
```

- **`--owner`** is the email of the person who owns the server. Required on the first `up`.
- **`--workspace NAME`** names the first workspace (default `my-team`).
- **`--port PORT`** changes the loopback port (default 8443).

The first `up`:
1. writes `~/.clawbridge-server/clawbridge.env`, mode 0600, with a random database password and the Linear app's
   token key. It's written once and never overwritten, and its values are never printed;
2. builds the image and starts the project `clawbridge-local`, publishing the API on `127.0.0.1:8443` only;
3. waits until `http://127.0.0.1:8443/healthz` answers, for up to 3 minutes;
4. shows the owner's one-time sign-in code in your terminal (`operator bootstrap-invite`), then the next command.

On the owner's computer (here, in shape A):

```sh
clawbridge join --server http://127.0.0.1:8443 --email owner@example.test
```

It asks for the code without showing it. Type it yourself.

- **The address is a number.** Clients join over plain http only to a numeric loopback address: use
  `http://127.0.0.1:8443`, never `http://localhost:8443`.
- **The code is a secret,** so it goes only to a terminal. Run from a pipe, a script or an agent, `up` skips it
  and tells you to run `deploy/local-host.sh invite-owner` in your own terminal. Use the same command if the code
  expires (24 hours) before the owner joins.
- **Only once.** A later `up` finds the workspace the owner already owns and makes no new code; it says so.
  `invite-owner` always makes one, which is also how an owner who lost every device gets back in. A new owner
  code revokes any earlier one still pending, so only the newest works.

## Shape B: your local network

Pick the name clients will use, such as `clawbridge.lan`, or use the machine's address (`192.0.2.10`). Each
client must resolve the name to this machine: an entry in your router's local DNS, or a hosts-file line
(`/etc/hosts`; on Windows `C:\Windows\System32\drivers\etc\hosts`):

```text
192.0.2.10  clawbridge.lan
```

Avoid names ending in `.local`, which belong to mDNS.

### B1: Caddy's own CA (the default)

```sh
deploy/local-host.sh up --owner owner@example.test --lan clawbridge.lan --bind 192.0.2.10
deploy/local-host.sh trust
```

- **`--bind`** is the address Caddy listens on, port 443. It defaults to `127.0.0.1`, so the server stays on
  this machine until you choose a LAN address. Use this machine's own LAN address: every interface
  (`0.0.0.0`, `::`) is refused.
- **Caddy signs the certificate** with a private CA it makes on the first start and keeps in its data volume.
  Its log says it "failed to install root certificate": expected, as it can't reach your OS's store from the
  container. You install it instead, with `trust`.
- **`trust`** copies the CA's root certificate to `~/.clawbridge-server/clawbridge-local-root.crt` and prints its
  name (`Caddy Local Authority - YEAR ECC Root`), its SHA-1 thumbprint and the commands below. Copy that file to
  every computer that signs in, and trust it there, after reading the warning under
  [Trusting the certificate](#trusting-the-certificate).
- **Then each person joins** with `clawbridge join --server https://clawbridge.lan --email EMAIL`.

The client trusts only the OS's certificate store. There is no option to pin a certificate or name a CA file.

### B2: mkcert

If you already use [mkcert](https://github.com/FiloSottile/mkcert), make a certificate for the name and bring it:

```sh
mkcert clawbridge.lan
deploy/local-host.sh up --owner owner@example.test --lan clawbridge.lan --bind 192.0.2.10 \
  --cert clawbridge.lan.pem --key clawbridge.lan-key.pem
```

Then trust mkcert's root (`mkcert -CAROOT` shows where it is) on each computer with the commands below.

### B3: Tailscale

Recommended when your team already uses Tailscale. The machine's tailnet name gets a publicly trusted
certificate, so no one installs a CA: run `tailscale cert NAME.TAILNET.ts.net` and pass the two files with
`--lan NAME.TAILNET.ts.net --bind` this machine's tailnet address, `--cert` and `--key`.

### Bringing a certificate

- **Copied in.** `--cert` and `--key` are copied into `~/.clawbridge-server/tls/`, which the script sets to 0700
  every time; the key is written 0600. Caddy's container runs as you, with no capability but binding 443, so it
  can read your key and nothing else of yours. It serves them with a Caddyfile it writes at start, and keeps no
  state: there is no CA to protect.
- **Renewing.** Run `up --lan HOST --cert FILE --key FILE` again with the new files: it copies them in and
  recreates Caddy's container, so it serves them. mkcert certificates last about two years; Tailscale's, 90 days.

### Trusting the certificate

**Read this before trusting Caddy's internal root (B1).** It has no name constraints: a computer that trusts it
accepts any certificate it signs, for any site, not only this server's. Its private key lives in the
`caddy-data` volume, so anyone who can read that volume (on most hosts, anyone who can use Docker there) can make
such a certificate. So:
- trust it only on the computers that sign in to this server;
- keep Docker access on the host to yourself;
- prefer Tailscale (B3: publicly trusted, nothing to install) or mkcert (B2) when either fits. mkcert's root has
  the same property, but you already trust it on your own machines;
- remove the trust when a computer stops using this server ([Untrusting it](#untrusting-it)).

| Where the client runs | Command |
|---|---|
| macOS | `security add-trusted-cert -d -r trustRoot -k ~/Library/Keychains/login.keychain-db clawbridge-local-root.crt` |
| Debian, Ubuntu | `sudo cp clawbridge-local-root.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates` |
| Fedora, RHEL | `sudo cp clawbridge-local-root.crt /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust` |
| Windows | `certutil -user -addstore Root clawbridge-local-root.crt`, or in PowerShell `Import-Certificate -FilePath clawbridge-local-root.crt -CertStoreLocation Cert:\CurrentUser\Root` |

- **On macOS,** the command opens a password dialog: trusting a root needs your approval on that Mac.
- **WSL and Windows have separate stores.** A `clawbridge` client inside WSL uses the Linux store; one on the
  Windows side uses the Windows store. Trust it in the one the client runs in.
- **On Linux,** `update-ca-certificates` reads only files ending in `.crt`.

### Untrusting it

A computer keeps trusting the root until you remove it there; uninstalling the server doesn't. `trust` prints
the root's name and SHA-1 thumbprint; for Caddy's, the name is `Caddy Local Authority - YEAR ECC Root`.

| Where the client runs | Command |
|---|---|
| macOS | `sudo security remove-trusted-cert -d clawbridge-local-root.crt`, then `security delete-certificate -Z SHA1 ~/Library/Keychains/login.keychain-db` (or by name: `security delete-certificate -c "Caddy Local Authority - 2026 ECC Root" ~/Library/Keychains/login.keychain-db`) |
| Debian, Ubuntu | `sudo rm /usr/local/share/ca-certificates/clawbridge-local-root.crt && sudo update-ca-certificates --fresh` |
| Fedora, RHEL | `sudo rm /etc/pki/ca-trust/source/anchors/clawbridge-local-root.crt && sudo update-ca-trust` |
| Windows | `certutil -user -delstore Root "Caddy Local Authority - 2026 ECC Root"` (or the serial number), or in PowerShell `Remove-Item Cert:\CurrentUser\Root\THUMBPRINT` |

On macOS, `trust` set the trust in the admin domain (`-d`), so the first command removes it from the same
domain (it asks for your password) before the second deletes the certificate. `remove-trusted-cert` reads the
certificate file: keep `clawbridge-local-root.crt`, or run `deploy/local-host.sh trust` again to get it back.

### Firewalls

- **macOS** may ask whether to let OrbStack or Docker accept connections. Allow it.
- **Linux:** open 443 to your network only, for example `sudo ufw allow from 192.0.2.0/24 to any port 443 proto tcp`.
- **WSL2:** a port published inside WSL reaches the Windows host, but not always the LAN. Docker Desktop
  publishes on Windows' own addresses; with Docker Engine in WSL you may need WSL's mirrored networking. Check
  from a second computer: `curl https://clawbridge.lan/healthz`.

## Inviting colleagues

Once the owner has joined, they invite each person from their own computer:

```sh
clawbridge invite --email ada@example.test --grant api=member
```

They send the person the invite link, or the code and the server's address, the way they'd send a password.
The person runs `clawbridge join` and pastes it ([Signing in](../sign-in.md)). In shape A, only people on this
machine can join.

## Backups

```sh
deploy/local-host.sh backup
```

This runs [`deploy/backup-postgres.sh`](../../deploy/backup-postgres.sh) against the running stack, as a cloud
server's backup does: a custom-format `pg_dump`, checked with `pg_restore --list`, then its SHA-256. It writes
the pair into `~/.clawbridge-server/backups/` (mode 0700, files 0600):

```text
20261029T023000Z.dump
20261029T023000Z.sha256
```

- **Retention.** It removes dump/checksum pairs older than 14 days from that folder, and nothing else. Set
  `CLAWBRIDGE_BACKUP_KEEP_DAYS` to keep more.
- **Keep a copy elsewhere.** A backup on the same disk doesn't survive that disk. Copy the folder to another
  drive or machine.
- **Keep the env file too.** `clawbridge.env` holds `CLAWBRIDGE_LINEAR_APP_TOKEN_KEY`, and
  `CLAWBRIDGE_MODEL_KEY_SEAL_KEY` if you added it. Without them, a restored database can't read the Linear
  app's tokens or the dispatcher model's keys. Keep a copy wherever you keep secrets; it isn't in the dump.

### Scheduling it

**macOS (launchd).** Save as `~/Library/LaunchAgents/local.clawbridge.backup.plist`, with your checkout's path
and a PATH that finds `docker` (OrbStack: `~/.orbstack/bin`; Docker Desktop: `/usr/local/bin`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>local.clawbridge.backup</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>/Users/YOU/src/ClawBridge/deploy/local-host.sh</string>
    <string>backup</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/Users/YOU/.orbstack/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>30</integer></dict>
  <key>StandardErrorPath</key><string>/Users/YOU/.clawbridge-server/backup.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/local.clawbridge.backup.plist
```

launchd runs a missed time when the Mac wakes.

**Linux (a systemd user timer).** `~/.config/systemd/user/clawbridge-local-backup.service`:

```ini
[Unit]
Description=ClawBridge local backup

[Service]
Type=oneshot
ExecStart=/bin/sh %h/src/ClawBridge/deploy/local-host.sh backup
```

and `~/.config/systemd/user/clawbridge-local-backup.timer`:

```ini
[Unit]
Description=ClawBridge local backup, daily

[Timer]
OnCalendar=*-*-* 02:30
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now clawbridge-local-backup.timer
loginctl enable-linger "$USER"   # so it runs while you're logged out
```

**Windows (Task Scheduler, running WSL).** In PowerShell or `cmd`, with your distribution's name and the
checkout's path inside WSL:

```text
schtasks /Create /SC DAILY /ST 02:30 /TN "ClawBridge backup" /TR "wsl.exe -d Ubuntu -e sh /home/YOU/src/ClawBridge/deploy/local-host.sh backup"
```

With Docker Desktop, it must be running at that time.

### The restore drill

Prove a backup restores, into a throwaway Postgres with no network, without touching the server:

```sh
set -eu
cd ~/.clawbridge-server/backups
STAMP=20261029T023000Z
shasum -a 256 -c "$STAMP.sha256"        # Linux and WSL: sha256sum -c "$STAMP.sha256"
CONTAINER=clawbridge-restore-drill-$$
docker run -d --rm --name "$CONTAINER" --network none \
  -e POSTGRES_HOST_AUTH_METHOD=trust postgres:17-alpine
trap 'docker rm -f -v "$CONTAINER" >/dev/null 2>&1 || true' 0
i=0
until docker exec "$CONTAINER" pg_isready -U postgres -d postgres; do
  i=$((i + 1)); [ "$i" -lt 30 ] || exit 1; sleep 1
done
docker exec -i "$CONTAINER" pg_restore --username=postgres --dbname=postgres \
  --no-owner --no-acl --exit-on-error <"$STAMP.dump"
docker exec "$CONTAINER" psql --username=postgres --dbname=postgres --tuples-only --no-align \
  --command "SELECT max(version) FROM clawbridge_schema;"
```

It prints the schema version, and the container goes when the shell ends. Run it after setting up the
schedule, and again every few months. [Backup and recovery](backup-and-recovery.md) has the reasoning and the
full restore.

### Restoring onto this machine

1. Stop the server: `deploy/local-host.sh down`.
2. Check the pair with the drill above.
3. From the checkout, start only the database, empty it and restore (with your folder and project name):

   ```sh
   cb() {
     docker compose -p clawbridge-local --env-file ~/.clawbridge-server/clawbridge.env \
       -f deploy/compose.yaml -f deploy/compose.local.yaml "$@"
   }
   cb up -d db
   cb exec -T db psql -U clawbridge -d clawbridge -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
   cb exec -T db pg_restore -U clawbridge -d clawbridge --no-owner --no-acl --exit-on-error \
     <~/.clawbridge-server/backups/STAMP.dump
   ```

4. `deploy/local-host.sh up`, then sign in from a client and check your workspace.

The env file must be the one the backup was taken with, or the Linear app's stored tokens are unreadable.

## Checking it

```sh
clawbridge cloud check all --local
```

It checks this machine's server from the same env file: health, settings and containers (`server`), the
certificate against this machine's trust store in shape B (`trust`), version, workspace, the Linear app, and
whether the newest backup is under 26 hours old (`backups`). It reads no secret and changes nothing. On Windows,
run it inside WSL. The [`cloud check` reference](../reference/api.md) lists every check and its reasons.

## Upgrades

```sh
git pull
deploy/local-host.sh backup
deploy/local-host.sh up
```

`up` rebuilds the image from the checkout and restarts what changed; the env file and the data stay. A new
release may migrate the database on its first start, one way only (the release notes say so), which is why the
backup comes first. Once ClawBridge publishes a server image, a later version of this script will pull it
instead of building.

## What needs a public URL

Nothing in either shape needs an inbound public address. Some things need more than this machine:
- **Colleagues off your network** need a tunnel to it, which is optional and up to you: Tailscale (B3 above), or
  Cloudflare Tunnel with a domain of your own. ClawBridge sets up neither.
- **Linear works on a LAN.** The server's watcher polls Linear (outbound only), and the Linear app's callback
  page (`ORIGIN/linear/callback`) only has to open in the browser of the person installing the app. Linear
  webhooks need a public https URL Linear can reach ([deployment](deployment.md#linear-webhooks-optional)), so
  a LAN-only server can't receive them; polling still works, on its usual cadence.
- **Dispatcher agents (agentd)** reach the server through an egress proxy that allows public addresses only,
  and never loopback, so they can't use shape A. With shape B, list the server's address with
  `CLAWBRIDGE_EGRESS_PRIVATE_SERVER=HOST:PORT`, and read [Installing clawbridge-agentd](agentd.md) first.
- **A sleeping laptop stops the server.** Clients can't reach it until it wakes; the messages they send wait in
  their outbox ([Usage](../usage.md)). For a team, run it on a machine that stays on.

## Stopping and uninstalling

```sh
deploy/local-host.sh status                    # the containers and the health check
deploy/local-host.sh down                      # stop; the data stays
deploy/local-host.sh up                        # start again with the saved settings
deploy/local-host.sh uninstall                 # also remove the built image; the data stays
deploy/local-host.sh uninstall --delete-data   # also delete the data, after you type the project name
```

`uninstall --delete-data` names the volumes it will delete and asks you to type the project name; `--yes`
confirms without asking. It never deletes `~/.clawbridge-server`: delete that yourself, once you have no use for
its backups and keys. A later `up` with the same folder starts an empty server with the same keys and invites
the owner again.

### Changing shape

The server's address is part of every device's sign-in, so changing it means every device joins again. `up`
refuses a flag that contradicts the saved settings rather than guess. To change on purpose, back up, then edit
`clawbridge.env` and run `up`:
- **A to B:** set `CLAWBRIDGE_LOCAL_SHAPE=lan`, `CLAWBRIDGE_DOMAIN=HOST`, `CLAWBRIDGE_PUBLIC_ORIGIN=https://HOST`
  and `CLAWBRIDGE_BIND_ADDR=IP`.
- **B1 to B2 or B3:** add `CLAWBRIDGE_TLS_CERT=~/.clawbridge-server/tls/cert.pem` and
  `CLAWBRIDGE_TLS_KEY=~/.clawbridge-server/tls/key.pem` (written out in full), then run
  `up --lan HOST --cert FILE --key FILE`.

## What this creates on your host

The names are for the default project, `clawbridge-local`, and folder, `~/.clawbridge-server`.

| Item | Exact name or path | What it holds | `down` | `uninstall` | `uninstall --delete-data` |
|---|---|---|---|---|---|
| Compose project | `clawbridge-local` (`CLAWBRIDGE_LOCAL_PROJECT` on the first `up`) | the label that groups everything below | stopped | removed | removed |
| Container | `clawbridge-local-db-1` | Postgres 17 | removed | removed | removed |
| Container | `clawbridge-local-api-1` | the ClawBridge server | removed | removed | removed |
| Container (brief) | `clawbridge-local-api-run-<id>` | `bootstrap-invite`, from `up` and `invite-owner` (`compose run --rm`): removed when it exits | – | – | – |
| Container (B1) | `clawbridge-local-caddy-1` | Caddy, with its internal CA | removed | removed | removed |
| Container (B2, B3) | `clawbridge-local-caddy-cert-1` | Caddy, with your certificate, running as your user | removed | removed | removed |
| Volume | `clawbridge-local_postgres-data` | the database: workspaces, projects, messages, devices | kept | kept | **deleted** |
| Volume (B1) | `clawbridge-local_caddy-data` | Caddy's CA (its root's private key) and certificates | kept | kept | **deleted** |
| Volume (B1) | `clawbridge-local_caddy-config` | Caddy's saved configuration | kept | kept | **deleted** |
| Network | `clawbridge-local_backend` | internal: the database's only network | removed | removed | removed |
| Network | `clawbridge-local_egress` | the API's and Caddy's way out (Linear, model providers) and the published ports | removed | removed | removed |
| Published port | `127.0.0.1:8443` → API (both shapes; `--port`) | shape A's address; shape B's health check | closed | closed | closed |
| Published port (shape B) | `BIND:443` → Caddy, `127.0.0.1` unless `--bind` | the server's https address | closed | closed | closed |
| Image | `clawbridge-local-api` | the server, built from your checkout | kept | removed | removed |
| Images (pulled) | `postgres:17-alpine`, `caddy:2.10-alpine` | shared base images | kept | kept | kept |
| Folder | `~/.clawbridge-server/` (0700 when the script makes it) | everything below | kept | kept | kept |
| Env file | `~/.clawbridge-server/clawbridge.env` (0600) | the database password, the Linear app's token key, the owner, the port, the shape | kept | kept | kept |
| Certificate copies (B2, B3) | `~/.clawbridge-server/tls/` (0700), `cert.pem`, `key.pem` (0600) | your certificate and its key | kept | kept | kept |
| Marker | `~/.clawbridge-server/owner-invited` | that the owner's code was shown | kept | kept | removed |
| Backups | `~/.clawbridge-server/backups/` (0700, files 0600) | dump/checksum pairs | kept | kept | kept |
| Root certificate (B1) | `~/.clawbridge-server/clawbridge-local-root.crt` | Caddy's CA root, from `trust` | kept | kept | kept |
| Client trust (B1, B2) | each client's OS store: the root you trusted there | lets that computer accept certificates the root signs | kept | kept | **kept until you remove it on each client** ([Untrusting it](#untrusting-it)) |

Docker's build cache keeps layers of the image build; `docker builder prune` clears it for every project on the
machine, so the script leaves it alone.
