# Commands (your own server over SSH)

Replace the capitalised placeholders with the user's inputs. Run the
`clawbridge` commands on the user's machine; they reach the host with the
user's own SSH.

## Reaching the host

Every command you run on the host goes through this `ssh`, with these options:

```sh
ssh -T -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=yes -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o PermitLocalCommand=no -o RemoteCommand=none HOST COMMAND
```

These are ClawBridge's own options, and they win over the user's
`~/.ssh/config`: no prompt, no host key accepted for the user, no agent, X11 or
port forwarding and no local or configured remote command, on a host where
commands run as root. `ssh HOST` below is short
for that command. If it fails with `Host key verification failed`, stop: the
user connects once with plain `ssh HOST` in their own terminal, compares the
fingerprint with their provider's console, and then you run it again.

**A block** (more than one command) goes as a file, as root, on `sh`'s stdin:
1. Write the block to a file in your working directory, such as `step3-disk.sh`.
2. Show it, and send it with the user's yes:

   ```sh
   ssh HOST sudo -n sh -s < step3-disk.sh
   ```

3. Report its output verbatim.

A block never contains a secret and never prints one (rule 3).

## 1. Preflight

```sh
ssh HOST 'hostname; uname -sm'
clawbridge cloud check infra --ssh HOST --server https://DOMAIN
ssh HOST 'sudo -n ss -Hltn "sport = :443"'
```

- **The first line.** Show it and get the user's yes that this is the machine
  (rule 1). Record `host HOSTNAME` in `created.txt`.
- **The check's reasons on a fresh host:** `data_not_mounted` (step 3 mounts the
  volume) and `port_closed` (nothing listens on 443 until step 6) are expected.
  `dns_unresolved` means the user's `A` record isn't there yet; they add it and
  you run the check again.
- **Out of scope** (stop with the advice): `docker_missing`, `compose_too_old`,
  `unsupported_platform`. Ask the user to install Docker Engine with the
  Compose plugin from https://docs.docker.com/engine/install/ for their
  distribution, then run the preflight again.
- **`sudo_needs_password`:** the user gives their SSH user passwordless sudo
  themselves. Out of scope.
- **The `ss` line** must print nothing. A listener on 443 belongs to something
  else on this machine: stop (rule 5).

## 2. Cost and the backup store

Show the estimate from [cost.md](cost.md) for the user's store and get their
yes. The user creates the bucket and its key in the store's console. **The
host's key may put and list, never delete,** because a host compromise is the
main reason for an off-host backup:

| Store | The host's key | And turn on | Retention |
|---|---|---|---|
| Backblaze B2 | an application key for this one bucket with `listFiles` and `writeFiles`, without `deleteFiles` | the bucket keeps all versions (B2's default); Object Lock if the account has it | a lifecycle rule on `postgres/daily/` that hides files after 30 days and deletes hidden ones after 1 |
| Cloudflare R2 | R2's object read & write token can delete, so it can't be scoped to put-only | a bucket lock rule on `postgres/` with a retention period of at least 30 days | an object lifecycle rule deleting `postgres/daily/` objects after 30 days |
| AWS S3, Wasabi, MinIO and other stores with S3 bucket policies | a policy allowing only `s3:PutObject` and `s3:ListBucket` on this bucket | versioning; Object Lock where offered | `deploy/s3-lifecycle.json` where the store accepts lifecycle rules |

The full table and the restore drill are in
`docs/operations/backup-and-recovery.md`. `cloud check backups` can't see a
key's rights, so ask the user to confirm the scope, and report their answer at
the end. If their store can neither scope a key this way nor lock or version
the bucket, stop (rule 5).

## 3. Prepare the host

**The data volume.** A separate block volume keeps the database through a
reinstall of the OS disk. Ask the user which device it is, then show what's on
it, and get their yes **naming the device** before the block:

```sh
ssh HOST 'lsblk -f DEVICE'
```

The block formats the device only when it has no filesystem, and adds the
`/etc/fstab` line only once, so running it again is safe.
- **After a format:** append `formatted DEVICE` to `created.txt`.
- **`REUSE=1`** only when `created.txt` already records `formatted DEVICE`.
- **If it prints STOP … already has a filesystem,** nothing was touched: the
  device holds data you didn't create. Stop and ask (rule 5).

```sh
set -e
REUSE=0
DISK=DEVICE
test -b "$DISK" || { echo "STOP: $DISK is not a block device"; exit 1; }
if blkid "$DISK" >/dev/null; then
  [ "$REUSE" = 1 ] || { echo "STOP: $DISK already has a filesystem; not formatting or mounting it"; exit 2; }
  echo "reusing the filesystem formatted on an earlier run"
else
  mkfs.ext4 -m 0 "$DISK"
fi
mkdir -p /var/lib/clawbridge-postgres
if ! grep -q ' /var/lib/clawbridge-postgres ' /etc/fstab; then
  u=$(blkid -s UUID -o value "$DISK")
  [ -n "$u" ]
  cp /etc/fstab /etc/fstab.clawbridge-pre-mount
  echo "UUID=$u /var/lib/clawbridge-postgres ext4 defaults,nofail 0 2" >> /etc/fstab
fi
mount -a
findmnt -n -o SOURCE /var/lib/clawbridge-postgres
readlink -f "$DISK"
```

The last two lines must print the same device.

**No separate volume.** Many small servers have none. Then the database lives
on the root disk, **only with the user's stated choice**. Tell them the cost:
reinstalling the OS means a restore from the off-host backups. With their yes,
step 4 adds `CLAWBRIDGE_DATA_ON_ROOT_DISK=1` to the env file. `cloud check
infra` then reports `warn data_on_root_disk` instead of failing, and step 6
skips the Docker mount guard.

**The firewall.** Only if the user wants you to enable one. Most providers also
have a firewall in their console, which the user sets: TCP 443 in, and their
SSH port. On the host, first read the SSH port this session actually uses and
whether a firewall is already on. Here too, `ssh HOST` means the full command
with every option from "Reaching the host":

```sh
ssh HOST 'echo "$SSH_CONNECTION" | cut -d " " -f 4; sudo -n ufw status 2>/dev/null || echo "no ufw"'
```

- **The first line is the SSH port.** Use exactly this number, never an assumed
  22: blocking it locks the user out of their own machine.
- **ufw is active:** add only the 443 rule with the user's yes:
  `ssh HOST 'sudo -n ufw allow 443/tcp'`. Never delete or change an existing rule.
- **ufw is installed but inactive, and the user wants it on:** this is its own
  approval. Show the exact rules, then send this block, with `SSH_PORT` the
  number above:

  ```sh
  set -e
  ufw allow SSH_PORT/tcp
  ufw allow 443/tcp
  ufw show added
  ufw --force enable
  ufw status
  ```

  Record `ufw enabled` in `created.txt`.
- **No ufw, or another firewall** (firewalld, nftables rules): stop and ask; the
  user configures it.

**The release.** Check out the tag of the user's own ClawBridge release, never a
branch. On the user's machine, `clawbridge version` prints its `version` (the
tag) and `commit`:

```sh
ssh HOST 'command -v git'
ssh HOST 'sudo -n git clone --branch vX.Y.Z --depth 1 https://github.com/jkai-chen/ClawBridge.git /opt/clawbridge/repo'
ssh HOST 'sudo -n git -C /opt/clawbridge/repo rev-parse HEAD'
```

Without `git`, stop: the user installs it from their distribution. **Check the
commit against the release, on the user's machine.** A tag can be moved
upstream, so the clone alone proves nothing. With the GitHub CLI:

```sh
gh release download vX.Y.Z --repo jkai-chen/ClawBridge --pattern SOURCE-COMMIT.txt --dir release-vX.Y.Z
gh attestation verify release-vX.Y.Z/SOURCE-COMMIT.txt --repo jkai-chen/ClawBridge \
  --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z
cat release-vX.Y.Z/SOURCE-COMMIT.txt
```

The verified `SOURCE-COMMIT.txt`, the `commit` from `clawbridge version` and the
clone's `rev-parse HEAD` must be the same commit. Without `gh`, compare the last
two and tell the user the attestation wasn't checked. Any difference: stop
(rule 5).

**The server image.** Each release publishes the server image, named by digest
in its `IMAGE-DIGEST.txt`, so the host pulls it instead of building. With `gh`,
verify it the same way, on the user's machine:

```sh
gh release download vX.Y.Z --repo jkai-chen/ClawBridge --pattern IMAGE-DIGEST.txt --dir release-vX.Y.Z
gh attestation verify "oci://$(cat release-vX.Y.Z/IMAGE-DIGEST.txt)" --repo jkai-chen/ClawBridge \
  --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml
cat release-vX.Y.Z/IMAGE-DIGEST.txt
ssh HOST 'docker compose version --short'
```

Use the image (`IMAGE` below is the file's one line,
`ghcr.io/jkai-chen/clawbridge@sha256:` and 64 hex digits) only when all of these
hold: the download and `gh attestation verify` both exit 0, the line has exactly
that form, and Compose prints 2.24 or later (the image file needs its
`!reset`). Anything else fails closed: no `gh`, a verify error, a missing or
malformed file, or an older Compose (say `2.20.2`) means **never set
`CLAWBRIDGE_IMAGE`**, and the server builds from the checked-out source, which
works the same. Tell the user which path it takes and why. Record
`image IMAGE` or `image: built from source (REASON)` in `created.txt`.

Done when `clawbridge cloud check infra --ssh HOST --server https://DOMAIN`
fails only with `port_closed`.

## 4. The env file

**The generated secrets,** made on the host. Run this before the first
`compose up`: a new database password doesn't match a database that already
exists.

```sh
clawbridge cloud env init --ssh HOST
```

It creates `/etc/clawbridge/clawbridge.env` (0600, root) if absent, adds
`POSTGRES_PASSWORD`, keeps any existing value, and prints key names only.

**The settings,** none of them secret, as a block. It adds each line only when
the file has no such key. Add `CLAWBRIDGE_DATA_ON_ROOT_DISK=1` to the list only
with the user's stated choice (step 3), and the sign-in lines from
[signin.md](signin.md):

```sh
set -e
f=/etc/clawbridge/clawbridge.env
if ! test -f "$f" || test -L "$f"; then echo "STOP: run cloud env init first"; exit 1; fi
[ -z "$(tail -c 1 "$f")" ] || echo >> "$f"
for kv in \
  CLAWBRIDGE_DOMAIN=DOMAIN \
  CLAWBRIDGE_BOOTSTRAP_OWNER=OWNER_EMAIL \
  CLAWBRIDGE_AUTH=device \
  CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data \
  CLAWBRIDGE_BACKUP_URL=s3://BUCKET \
  CLAWBRIDGE_BACKUP_REGION=REGION \
  CLAWBRIDGE_BACKUP_ENDPOINT=ENDPOINT \
  COMPOSE_PROJECT_NAME=clawbridge
do
  k=${kv%%=*}
  if grep -q "^$k=" "$f"; then echo "kept $k"; else printf '%s\n' "$kv" >> "$f"; echo "added $k"; fi
done
```

**The image line** is this run's, never kept from an earlier one: an upgrade
must not pull the previous release, and a run that fails closed must build. On
every run, set `IMAGE=` to the verified line from step 3 ("The server image"),
or leave it empty when the image didn't verify:

```sh
set -e
f=/etc/clawbridge/clawbridge.env
IMAGE=
sed -i '/^CLAWBRIDGE_IMAGE=/d' "$f"
if [ -n "$IMAGE" ]; then printf 'CLAWBRIDGE_IMAGE=%s\n' "$IMAGE" >> "$f"; echo "set CLAWBRIDGE_IMAGE"; else echo "removed CLAWBRIDGE_IMAGE"; fi
```

**The backup store's key** is the user's secret. Stop and ask them to run, in
their own terminal:

```sh
clawbridge cloud secret set CLAWBRIDGE_BACKUP_ACCESS_KEY_ID --ssh HOST
clawbridge cloud secret set CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY --ssh HOST
```

Done when this prints `1` for each key:

```sh
ssh HOST 'for k in POSTGRES_PASSWORD CLAWBRIDGE_DOMAIN CLAWBRIDGE_BOOTSTRAP_OWNER CLAWBRIDGE_BACKUP_URL CLAWBRIDGE_BACKUP_ENDPOINT CLAWBRIDGE_BACKUP_ACCESS_KEY_ID CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY COMPOSE_PROJECT_NAME; do printf "%s " $k; sudo -n grep -c "^$k=." /etc/clawbridge/clawbridge.env; done'
```

## 6. Install the server

**Other containers.** The Docker mount guard below restarts Docker when it is
new or changed, which restarts every container on this machine. Show them first
and get the user's yes:

```sh
ssh HOST 'sudo -n docker ps -a --format "{{.Names}} {{.Status}}"'
```

**The disk guard** (a separate volume only; skip it with
`CLAWBRIDGE_DATA_ON_ROOT_DISK=1`). It makes Docker start only with the data
volume mounted:

```sh
set -e
src=/opt/clawbridge/repo/deploy/systemd/docker.service.d/10-clawbridge-postgres-mount.conf
dst=/etc/systemd/system/docker.service.d/10-clawbridge-postgres-mount.conf
if ! cmp -s "$src" "$dst"; then
  install -d -o root -g root -m 0755 /etc/systemd/system/docker.service.d
  install -o root -g root -m 0644 "$src" "$dst"
  systemctl daemon-reload
  systemctl restart docker
fi
systemctl is-active docker
```

**The stack and the backup timer.** On a first install nothing is running yet.
If the server already runs (this playbook run again, to repair or upgrade it),
the block drains it first, as deployment.md's "Draining before the swap" does:
writes get `503 maintenance` and clients queue them, nothing in flight is cut,
and the new API ends the maintenance when it starts. The drain must print
`drained`; if it prints `not drained after 90s`, the block stops before `up`:
stop and ask (rule 5), and `operator maintenance end --confirm` (the same `run`
line) lifts it. With `CLAWBRIDGE_IMAGE` in the env file the block pulls that
image (adding `compose.image.yaml`); without it, it builds from the checkout.
Either happens before the drain, so the server is paused only for the swap.

```sh
set -e
mkdir -p /var/lib/clawbridge-postgres/data
env=/etc/clawbridge/clawbridge.env
image=
grep -q '^CLAWBRIDGE_IMAGE=.' "$env" && image="-f /opt/clawbridge/repo/deploy/compose.image.yaml"
dc() { docker compose --env-file "$env" --project-name clawbridge \
  -f /opt/clawbridge/repo/deploy/compose.yaml $image "$@"; }
if [ -n "$image" ]; then dc pull api; else dc build api; fi
if [ -n "$(dc ps -q api)" ]; then
  dc run --rm --no-deps api operator maintenance start --reason "playbook re-run" --wait 90s --confirm
fi
dc up -d
install -d -o root -g root -m 0755 /usr/local/libexec/clawbridge
install -o root -g root -m 0755 /opt/clawbridge/repo/deploy/backup-postgres.sh \
  /usr/local/libexec/clawbridge/backup-postgres.sh
install -o root -g root -m 0644 /opt/clawbridge/repo/deploy/systemd/clawbridge-postgres-backup.service \
  /etc/systemd/system/clawbridge-postgres-backup.service
install -o root -g root -m 0644 /opt/clawbridge/repo/deploy/systemd/clawbridge-postgres-backup.timer \
  /etc/systemd/system/clawbridge-postgres-backup.timer
systemctl daemon-reload
systemctl enable --now clawbridge-postgres-backup.timer
```

Record `compose project clawbridge`, the two units and the drop-in in
`created.txt`. Caddy obtains the certificate within a minute or two, once DNS
points at the host and 443 is open.

**The first backup,** so the backups check has something to see:

```sh
ssh HOST 'sudo -n systemctl start clawbridge-postgres-backup.service; sudo -n systemctl status clawbridge-postgres-backup.service --no-pager'
```

Done when this passes (a `warn` line passes; report it):

```sh
clawbridge cloud check infra server version backups --ssh HOST --server https://DOMAIN
```
