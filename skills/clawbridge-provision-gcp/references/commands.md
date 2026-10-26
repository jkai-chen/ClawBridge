# Commands, step by step

Replace the capitalised placeholders with the user's inputs: `PROJECT`, `REGION`,
`ZONE`, `DOMAIN`, `OWNER_EMAIL`, `SIZE`, `BUCKET`. The resource names
(`clawbridge-1`, `clawbridge-ip`, `clawbridge-data`, `clawbridge-vm`,
`clawbridge-https`, `clawbridge-iap-ssh`) are fixed: the checks and the guide
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

The static IP, the data disk and the firewall. Only TCP 443 is public; SSH
comes only from Google's IAP range:

```sh
gcloud compute addresses create clawbridge-ip --project=PROJECT --region=REGION
gcloud compute disks create clawbridge-data --project=PROJECT --zone=ZONE --size=20GB --type=pd-balanced
gcloud compute firewall-rules create clawbridge-https --project=PROJECT --network=default \
  --direction=INGRESS --allow=tcp:443 --source-ranges=0.0.0.0/0 --target-tags=clawbridge
gcloud compute firewall-rules create clawbridge-iap-ssh --project=PROJECT --network=default \
  --direction=INGRESS --allow=tcp:22 --source-ranges=35.235.240.0/20 --target-tags=clawbridge
```

The VM, with the data disk attached and kept if the VM is ever deleted:

```sh
gcloud compute instances create clawbridge-1 --project=PROJECT --zone=ZONE \
  --machine-type=SIZE --image-family=debian-12 --image-project=debian-cloud \
  --boot-disk-size=20GB --boot-disk-type=pd-balanced --address=clawbridge-ip \
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

**The data disk.** Format it only when it is new and empty: `blkid` must print
nothing. If it prints a filesystem, stop and ask (rule 5).

```sh
DISK=/dev/disk/by-id/google-clawbridge-data
sudo blkid "$DISK"
sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard "$DISK"
sudo mkdir -p /var/lib/clawbridge-postgres
echo "UUID=$(sudo blkid -s UUID -o value "$DISK") /var/lib/clawbridge-postgres ext4 discard,defaults,nofail 0 2" |
  sudo tee -a /etc/fstab >/dev/null
sudo mount -a
findmnt -n -o SOURCE /var/lib/clawbridge-postgres
readlink -f "$DISK"
```

The last two lines must print the same device: the mount is the data disk.

**Docker,** from Docker's own Debian repository:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" |
  sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

**The release.** Check out the tag of the user's own ClawBridge release (the
`version` that `clawbridge version` prints on their machine), never a branch,
and record its commit:

```sh
TAG=vX.Y.Z
sudo git clone --branch "$TAG" --depth 1 https://github.com/jkai-chen/ClawBridge.git /opt/clawbridge/repo
sudo git -C /opt/clawbridge/repo rev-parse HEAD
```

## 6. Install the server

**The env file,** created once, root-only. The database password is generated
here and never printed. Add the sign-in lines from
[signin.md](signin.md) for the chosen path (the device path adds
`CLAWBRIDGE_AUTH=device`):

```sh
sudo install -d -o root -g root -m 0755 /etc/clawbridge
sudo test ! -e /etc/clawbridge/clawbridge.env
sudo sh -c 'umask 077; {
  printf "POSTGRES_PASSWORD=%s\n" "$(openssl rand -hex 32)"
  printf "CLAWBRIDGE_DOMAIN=%s\n" DOMAIN
  printf "CLAWBRIDGE_BOOTSTRAP_OWNER=%s\n" OWNER_EMAIL
  printf "CLAWBRIDGE_POSTGRES_DATA_PATH=/var/lib/clawbridge-postgres/data\n"
  printf "CLAWBRIDGE_BACKUP_BUCKET=%s\n" BUCKET
  printf "COMPOSE_PROJECT_NAME=clawbridge\n"
} > /etc/clawbridge/clawbridge.env'
```

If `test ! -e` fails, an env file already exists: stop and ask (rule 5).

**The disk guard and backups.** Follow "Persistent disk and backup guards" in
`/opt/clawbridge/repo/docs/operations/deployment.md`, skipping its `/etc/fstab`
and symlink parts, which this playbook already did. It makes Docker start only
with the data disk mounted and installs the daily backup timer.

**Start the stack:**

```sh
sudo docker compose --env-file /etc/clawbridge/clawbridge.env --project-name clawbridge \
  -f /opt/clawbridge/repo/deploy/compose.yaml up -d --build
```

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
