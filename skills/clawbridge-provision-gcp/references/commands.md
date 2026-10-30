# Commands, step by step

Replace the capitalised placeholders with the user's inputs: `PROJECT`, `REGION`,
`ZONE`, `DOMAIN`, `OWNER_EMAIL`, `SIZE`, `BUCKET`. The resource names
(`clawbridge-1`, `clawbridge-ip`, `clawbridge-data`, `clawbridge-vm`,
`clawbridge-net`, `clawbridge-subnet`, `clawbridge-https`, `clawbridge-iap-ssh`)
are fixed: the checks and the guide
expect them. Append each name to `created.txt` right after its create succeeds.

## 1. Preflight

```sh
gcloud config list account --format='value(core.account)'
gcloud projects describe PROJECT --format='value(projectId,lifecycleState)'
gcloud billing projects describe PROJECT --format='value(billingEnabled)'
gcloud services list --enabled --project=PROJECT --format='value(config.name)'
```

The account must be the one the user named, the project `ACTIVE`, billing
`True`, and the services list must include `compute.googleapis.com`,
`storage.googleapis.com` and `iap.googleapis.com`. A missing API: stop and ask
(rule 5); with the user's yes, `gcloud services enable NAME --project=PROJECT`.

Then confirm none of the names exist yet. Each command must print nothing:

```sh
gcloud compute instances list --project=PROJECT --filter='name=clawbridge-1' --format='value(name)'
gcloud compute addresses list --project=PROJECT --filter='name=clawbridge-ip' --format='value(name)'
gcloud compute disks list --project=PROJECT --filter='name=clawbridge-data' --format='value(name)'
gcloud compute firewall-rules list --project=PROJECT --filter='name~^clawbridge-' --format='value(name)'
gcloud compute networks list --project=PROJECT --filter='name=clawbridge-net' --format='value(name)'
gcloud iam service-accounts list --project=PROJECT --filter='email~^clawbridge-vm@' --format='value(email)'
gcloud storage buckets list --project=PROJECT --filter='name=BUCKET' --format='value(name)'
```

## 3. Provision

The VM's identity, allowed only to write backups into the bucket:

```sh
gcloud iam service-accounts create clawbridge-vm --project=PROJECT --display-name='ClawBridge VM'
gcloud storage buckets create gs://BUCKET --project=PROJECT --location=REGION \
  --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets add-iam-policy-binding gs://BUCKET \
  --member=serviceAccount:clawbridge-vm@PROJECT.iam.gserviceaccount.com \
  --role=roles/storage.objectCreator
```

Keep 30 days of backups. Write `lifecycle.json` in your working directory:

```json
{"rule": [{"action": {"type": "Delete"}, "condition": {"age": 30, "matchesPrefix": ["postgres/daily/"]}}]}
```

```sh
gcloud storage buckets update gs://BUCKET --lifecycle-file=lifecycle.json
```

A dedicated network, so no project-wide rule (such as the `default` network's
SSH, RDP and ICMP from anywhere) reaches the VM. Its only ingress: TCP 443 from
anywhere, and SSH from Google's IAP range. Outbound traffic is allowed, as on
any VPC:

```sh
gcloud compute networks create clawbridge-net --project=PROJECT --subnet-mode=custom
gcloud compute networks subnets create clawbridge-subnet --project=PROJECT --network=clawbridge-net \
  --region=REGION --range=10.20.0.0/24
gcloud compute firewall-rules create clawbridge-https --project=PROJECT --network=clawbridge-net \
  --direction=INGRESS --allow=tcp:443 --source-ranges=0.0.0.0/0 --target-tags=clawbridge
gcloud compute firewall-rules create clawbridge-iap-ssh --project=PROJECT --network=clawbridge-net \
  --direction=INGRESS --allow=tcp:22 --source-ranges=35.235.240.0/20 --target-tags=clawbridge
```

Check: the network's rules must be exactly these two.

```sh
gcloud compute firewall-rules list --project=PROJECT --filter='network~/clawbridge-net$' \
  --format='value(name,sourceRanges.list(),allowed[].map().firewall_rule().list())'
```

The static IP and the data disk:

```sh
gcloud compute addresses create clawbridge-ip --project=PROJECT --region=REGION
gcloud compute disks create clawbridge-data --project=PROJECT --zone=ZONE --size=20GB --type=pd-balanced
```

The VM, with the data disk attached and kept if the VM is ever deleted:

```sh
gcloud compute instances create clawbridge-1 --project=PROJECT --zone=ZONE \
  --machine-type=SIZE --image-family=debian-12 --image-project=debian-cloud \
  --boot-disk-size=20GB --boot-disk-type=pd-balanced --address=clawbridge-ip \
  --network=clawbridge-net --subnet=clawbridge-subnet \
  --disk=name=clawbridge-data,device-name=clawbridge-data,mode=rw,auto-delete=no \
  --tags=clawbridge --service-account=clawbridge-vm@PROJECT.iam.gserviceaccount.com \
  --scopes=cloud-platform --shielded-secure-boot --metadata=enable-oslogin=TRUE
```

Show the user the IP and ask them to create an `A` record for `DOMAIN` with it
at their DNS provider:

```sh
gcloud compute addresses describe clawbridge-ip --project=PROJECT --region=REGION --format='value(address)'
```

## 4. Prepare the VM

Open a shell on the VM over IAP. The commands in this step and step 6 run
there:

```sh
gcloud compute ssh clawbridge-1 --project=PROJECT --zone=ZONE --tunnel-through-iap
```

**The data disk.** The block formats the disk only when it has no filesystem,
and adds the `/etc/fstab` line only once, so running it again is safe. After a
format, append `formatted clawbridge-data` to `created.txt`. If it prints STOP
and `created.txt` does not record that format, the disk holds data you did not
create: stop and ask (rule 5).

```sh
DISK=/dev/disk/by-id/google-clawbridge-data
if sudo blkid "$DISK" >/dev/null; then
  echo "STOP: $DISK already has a filesystem; not formatting"
else
  sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$DISK"
fi
sudo mkdir -p /var/lib/clawbridge-postgres
if ! grep -q ' /var/lib/clawbridge-postgres ' /etc/fstab; then
  echo "UUID=$(sudo blkid -s UUID -o value "$DISK") /var/lib/clawbridge-postgres ext4 discard,defaults,nofail 0 2" |
    sudo tee -a /etc/fstab >/dev/null
fi
sudo mount -a
findmnt -n -o SOURCE /var/lib/clawbridge-postgres
readlink -f "$DISK"
```

The last two lines must print the same device: the mount is the data disk.

**Docker,** from Docker's own Debian repository. Its signing key is checked
against Docker's published fingerprint before apt trusts it:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git gnupg
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
FPR=$(gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc | awk -F: '/^fpr/ {print $10; exit}')
if [ "$FPR" != 9DC858229FC7DD38854AE2D88D81803C0EBFCD88 ]; then
  sudo rm -f /etc/apt/keyrings/docker.asc
  echo "STOP: Docker's apt key has fingerprint ${FPR:-none}, not 9DC8 5822 9FC7 DD38 854A E2D8 8D81 803C 0EBF CD88"
else
  sudo chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" |
    sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
  sudo apt-get update
  sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
```

If it prints STOP, nothing from Docker's repository was trusted or installed:
stop and ask (rule 5).

**The release.** Check out the tag of the user's own ClawBridge release, never
a branch. On the user's machine, `clawbridge version` prints its `version` (the
tag) and `commit`:

```sh
TAG=vX.Y.Z
sudo git clone --branch "$TAG" --depth 1 https://github.com/jkai-chen/ClawBridge.git /opt/clawbridge/repo
sudo git -C /opt/clawbridge/repo rev-parse HEAD
```

The printed commit must equal the `commit` from `clawbridge version`. If it
differs, stop and ask (rule 5).

## 6. Install the server

**The env file,** created once, root-only. The database password is generated
here and never printed. Add the sign-in lines from
[signin.md](signin.md) for the chosen path (the device path adds
`CLAWBRIDGE_AUTH=device`):

```sh
sudo install -d -o root -g root -m 0755 /etc/clawbridge
if sudo test -e /etc/clawbridge/clawbridge.env; then
  echo "STOP: /etc/clawbridge/clawbridge.env already exists; not replacing it"
else
  sudo sh -c 'umask 077; {
  printf "POSTGRES_PASSWORD=%s\n" "$(openssl rand -hex 32)"
  printf "CLAWBRIDGE_DOMAIN=%s\n" DOMAIN
  printf "CLAWBRIDGE_BOOTSTRAP_OWNER=%s\n" OWNER_EMAIL
  printf "CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data\n"
  printf "CLAWBRIDGE_BACKUP_BUCKET=%s\n" BUCKET
  printf "COMPOSE_PROJECT_NAME=clawbridge\n"
} > /etc/clawbridge/clawbridge.env'
fi
```

If it prints STOP, an env file already exists: stop and ask (rule 5). Add
further lines with `sudo tee -a`, never by rewriting the file.

**The disk guard and backups.** Follow "Persistent disk and backup guards" in
`/opt/clawbridge/repo/docs/operations/deployment.md`, skipping its `/etc/fstab`
and symlink parts, which this playbook already did. It makes Docker start only
with the data disk mounted and installs the daily backup timer.

**Start the stack.** On a first install nothing is running yet. If the server
already runs (this playbook run again, to repair or upgrade it), the block
drains it first, as deployment.md's "Draining before the swap" does: writes get
`503 maintenance` and clients queue them, nothing in flight is cut, and the new
API ends the maintenance when it starts. The drain must print `drained`; if it
prints `not drained after 90s`, the block stops before `up`: stop and ask
(rule 5), and `operator maintenance end --confirm` (the same `run` line) lifts
it.

```sh
(
set -e
dc() { sudo docker compose --env-file /etc/clawbridge/clawbridge.env --project-name clawbridge \
  -f /opt/clawbridge/repo/deploy/compose.yaml "$@"; }
if [ -n "$(dc ps -q api)" ]; then
  dc build api
  dc run --rm --no-deps api operator maintenance start --reason "playbook re-run" --wait 90s --confirm
fi
dc up -d --build
)
```

The parentheses keep `set -e` to this block: in the interactive session from
step 4, a failed drain stops the block, not the session.

Caddy obtains the certificate within a minute or two, once DNS points at the
IP. Then, from the user's machine:

```sh
clawbridge cloud check server version --server https://DOMAIN \
  --vm clawbridge-1 --project PROJECT --zone ZONE
```

**The first backup,** so the backups check has something to see:

```sh
sudo systemctl start clawbridge-postgres-backup.service
```
