# First deployment

This guide takes a team from nothing to its own running ClawBridge server, a
first project and the Linear app, on **Google Cloud**, **AWS** or **a Linux
server you already have**, reached over your own SSH. It is the
written path behind Start's "Set up my own server" (`clawbridge onboard`, choice
3, which then asks where: Google Cloud, AWS, this machine or your LAN, or a
server you already have), for people who do the steps themselves rather than
hand them to a coding agent. The "this machine or your LAN" answer runs the
server with Docker on your own machine, with no cloud; its guide is
[local hosting](local-host.md), and its playbook
`clawbridge skill install --playbook local`.

It follows a fixed sequence of steps. A step is done when its check passes, never
when someone says it is. The checks are deterministic, read no secret values and
change nothing:

```sh
clawbridge cloud check infra              # after the server is provisioned
clawbridge cloud check server version     # after the server is installed
clawbridge cloud check workspace          # after the owner joins
clawbridge cloud check backups            # after the first backup run
```

To run the server on your own machine instead, see local-host.md; its checks are
`clawbridge cloud check --local` (docs/reference/api.md, "Cloud check and secret entry").

The exact commands for each cloud are in its provisioning playbook, with its
cost table and the checks for each step:
- Google Cloud: `clawbridge skill install --playbook gcp`. The server is a
  Compute Engine VM reached over IAP, and its backups go to Cloud Storage.
- AWS: `clawbridge skill install --playbook aws`. The server is an EC2 instance
  reached over SSM, with no SSH, and its backups go to S3.
- Your own server: `clawbridge skill install --playbook vm`. The server is a
  Linux host you reach over your own SSH, and its backups go to S3-compatible
  storage off the host.

No cloud account and no server to reach over SSH? To run the server on this computer, for yourself or
for a team on one network, see [Running the server on your own machine](local-host.md).

This guide names what each step
needs and links to the [deployment guide](deployment.md) for the server itself.

Examples use `example.com` and `my-team`, and:
- **Google Cloud:** a project named `example-project`, in zone `europe-west1-b`;
- **AWS:** account `123456789012`, region `eu-west-1`, an AWS CLI profile named
  `clawbridge-onboarding` and an instance `i-0123456789abcdef0`;
- **your own server:** an SSH host `admin@203.0.113.7`, and a backup store at
  `https://s3.eu-central-003.example.com`.

## Which cloud

All three run the same server. The table shows what differs.

| | Google Cloud | AWS | Your own server |
|---|---|---|---|
| Server | a Compute Engine VM | an EC2 instance (Amazon Linux 2023, arm64) | your Linux host, amd64 or arm64, with Docker Engine and Compose v2.24 or later already installed |
| Your admin access | SSH through IAP, as your Google account | SSM, as your named AWS CLI profile, with no SSH port at all | your own SSH, with key-based login as a user with passwordless sudo |
| Your CLI | `gcloud`, with a named configuration | AWS CLI v2, with a named profile you set up (`aws configure sso --profile clawbridge-onboarding`) | `ssh`, with your own `~/.ssh/config` |
| Backups | a Cloud Storage bucket | an S3 bucket | S3-compatible storage off the host (Backblaze B2, Cloudflare R2, S3 and the like), with a key that can't delete |
| Where you run it | wherever `gcloud` runs | macOS, Linux or WSL (the AWS route reads its requests from `/dev/stdin`); on Windows, [set up WSL first](../installation.md#windows-set-up-wsl-for-the-aws-route) | wherever `ssh` runs |
| Monthly estimate | about $21.50 (`e2-small`) or $34 (`e2-medium`) | about $19 (`t4g.small`) or $31.50 (`t4g.medium`) | your host's own cost, plus the backup store (often free under 10 GB) |

**Your own server** is not yet verified end to end on a live non-GCP/AWS host; the
steps are tested with a fake SSH transport. **Google Cloud and AWS** were rehearsed
step by step, not run end to end on a fresh account. If you find a gap, please report it.

ClawBridge never creates or edits your CLI profile or configuration, and never
reads `~/.aws` or the gcloud credential store.

## Rules that hold throughout

- **Name everything explicitly.** Every cloud command names the project, region
  and zone on Google Cloud, `--profile` and `--region` on AWS, and the host
  (`--ssh HOST`) on your own server. Never rely on
  a CLI's default project, profile or region.
- **Cost first.** Before creating anything billable, write down the itemised
  monthly estimate: the VM type, disk size, static IP, egress assumption and
  backup storage. The estimate is yours to approve; anything added later needs a
  new estimate.
- **Secrets are generated on the server, or entered by you.** Database passwords
  and token keys are generated on the server (`clawbridge cloud env init`, or
  `openssl rand`) straight into the
  root-only environment file and never printed. Secrets you hold, such as the
  Linear app's client secret, are entered with `clawbridge cloud secret set KEY`.
  It reads the value with hidden input, so it never appears in a command line, a
  log or an agent's transcript. How it reaches the server:
  - **Over SSH (Google Cloud, or any host):** on stdin of your own SSH session.
  - **On AWS:** SSM keeps every command and its output in the account's history,
    so the value never travels in a command. ClawBridge writes it to a
    `SecureString` parameter, `/clawbridge/<instance ID>/<KEY>`, which expires
    after 15 minutes. The instance reads that one parameter with its own role,
    and ClawBridge deletes it straight after, whatever happened. If one is ever
    left behind, `cloud check infra` fails with `secret_left_behind` and names
    it (never its value). The details are in the
    [CLI reference](../reference/api.md).
- **Never print the environment file.** To check that a key is set, count it:
  `sudo grep -c '^KEY=.\+' /etc/clawbridge/clawbridge.env`.

## 1. Choose the cloud, account, region and domain

You need:
- an account you can bill: a Google Cloud project, or an AWS account;
- a region close to your team;
- a domain or subdomain whose DNS you control (for example
  `clawbridge.example.com`).

On AWS, also set up a named AWS CLI profile signed in to that account, for
example with `aws configure sso --profile clawbridge-onboarding`.
- **For the checks and `secret set`:** the profile needs the permissions in
  [`deploy/aws-profile-policy.json`](../../deploy/aws-profile-policy.json). That
  is read-only describes, a listing of the backup prefix, and SSM send-command
  on the one instance with its own parameters.
- **For provisioning:** this needs more (EC2, IAM and S3 create calls), which
  the AWS playbook names.

On your own server you need, before anything else:
- **The host:** 64-bit Linux, amd64 or arm64, with a public address.
- **Packages:** Docker Engine with the Compose plugin, v2.24 or later, and `git`.
  Neither ClawBridge nor its playbook installs Docker, upgrades the OS or
  changes SSH or sudo; the preflight names what's missing.
- **Access:** key-based SSH as a user with passwordless sudo (`sudo -n`). Connect
  once with plain `ssh HOST` and compare the host key's fingerprint with your
  provider's console: ClawBridge never accepts a host key for you.

## 2. Estimate the cost

A small server is enough for a team. The playbooks carry a fixed estimate table
per size and cloud.
- **Google Cloud:** about $21.50 a month on `e2-small` and $34 on `e2-medium`, with its
  disk, static IP and backups.
- **AWS (`us-east-1`):** `t4g.small` is about $19 a month and `t4g.medium` about
  $31.50, with the encrypted data volume, Elastic IP and S3 backups. Other
  regions cost up to about 25% more. The Elastic IP is billed while allocated,
  even with the instance stopped.
- **Your own server:** the host's cost is yours already. The backup store is the
  new bill: about 5 GB is free on B2 and R2, about $0.12 a month on S3, and at
  least about $7 on Wasabi.

## 3. Provision

Create, with the names recorded as you go:

- **A server:** a VM with Docker and the Compose plugin.
  - **AWS:** an EC2 instance from Amazon Linux 2023, arm64, which ships the SSM
    agent and the AWS CLI. Its root volume is encrypted, and it requires
    IMDSv2.
- **A static address:** a static IP attached to the server, and a DNS `A` record
  for the domain pointing at it.
  - **AWS:** an Elastic IP associated with the instance.
- **A firewall rule** opening only TCP 443 to the server (Caddy obtains its
  certificate over TLS on 443 and serves no plain HTTP; see
  [HTTPS and operator access](deployment.md#https-and-operator-access)).
  - **Google Cloud:** an administrative path for SSH, through IAP.
  - **AWS:** a security group with TCP 443 and nothing else. There is no SSH
    port: administration is through SSM.
- **A data disk:** a separate persistent disk for the database, mounted at
  `/var/lib/clawbridge-postgres` (see
  [persistent disk and backup guards](deployment.md#persistent-disk-and-backup-guards)).
  - **AWS:** an encrypted EBS volume in the instance's zone.
- **A backup bucket:** private, and an identity for the server that may write
  to it (see [backup and recovery](backup-and-recovery.md)).
  - **AWS:** a private, versioned, encrypted S3 bucket keeping 30 days of
    backups, using [`deploy/s3-lifecycle.json`](../../deploy/s3-lifecycle.json).
    The instance role carries
    [`deploy/aws-instance-role-policy.json`](../../deploy/aws-instance-role-policy.json):
    - `s3:PutObject` under `postgres/daily/` in that bucket;
    - `ssm:GetParameter` on its own `/clawbridge/<instance ID>/*` only;
    - `kms:Decrypt` only through SSM.

    Beside it goes the AWS-managed `AmazonSSMManagedInstanceCore` for the SSM
    agent.

**On your own server** there is no server to create. Prepare it:
- **The address:** a DNS `A` (or `AAAA`) record for the domain pointing at the
  host's public address.
- **The firewall:** TCP 443 in, in your provider's console and on the host.
  Before enabling a host firewall such as ufw, allow the SSH port your session
  actually uses (`echo "$SSH_CONNECTION" | cut -d " " -f 4` on the host), never
  an assumed 22: blocking it locks you out of your own machine.
- **A data disk:** a separate block volume mounted at
  `/var/lib/clawbridge-postgres`, by UUID in `/etc/fstab`, as on the clouds.
  Many small servers have none: then add `CLAWBRIDGE_DATA_ON_ROOT_DISK=1` to the
  env file to keep the database on the root disk by your stated choice. The
  infra check then warns `data_on_root_disk` instead of failing, and a
  reinstall of the OS means a restore from the off-host backups.
- **A backup bucket** in S3-compatible storage off the host, with a key for the
  host that may put and list, never delete, and versioning or a lock on the
  bucket. The table per store is in
  [backup and recovery](backup-and-recovery.md).

On Google Cloud, connect once first with `gcloud compute ssh clawbridge-1 --project …
--zone … --tunnel-through-iap` in your own terminal (it makes your key), with a firewall
rule allowing `35.235.240.0/20` on tcp:22 (IAP); `cloud check --vm` refuses until both exist.

On Google Cloud and AWS, done when the infra check fails only with `data_not_mounted`;
the next step mounts the volume. On your own server, where you mounted it (or set
`CLAWBRIDGE_DATA_ON_ROOT_DISK=1`) already, done when it fails only with `port_closed`.

```sh
# Google Cloud
clawbridge cloud check infra --vm clawbridge-1 --project example-project --zone europe-west1-b --server https://clawbridge.example.com
# AWS
clawbridge cloud check infra --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012 --server https://clawbridge.example.com
# Your own server
clawbridge cloud check infra --ssh admin@203.0.113.7 --server https://clawbridge.example.com
```

On your own server, the check reads the host itself: the platform, Docker and
Compose, the env file's mode, the data mount and its free space, the clock, DNS
and 443. A domain whose addresses aren't on the host (normal behind 1:1 NAT) is
a `warn`, which passes.

On AWS, `ssm_offline` is normal for a few minutes right after the instance
starts: it has no public IP until the Elastic IP is associated, so it can't
reach SSM. Wait and run the check again before counting it as a failure.

## 4. Prepare the server

Format and mount the data volume (a new volume only), install Docker and the
Compose plugin, and check out the release. Done when the same infra check fails
only with `port_closed`: nothing listens on 443 until the server is installed.

**On your own server,** Docker is already there. Check out the release's tag at
`/opt/clawbridge/repo` and compare its commit, `git -C /opt/clawbridge/repo
rev-parse HEAD` on the host, with the release's `SOURCE-COMMIT.txt`, verified on
your own computer with `gh attestation verify` (see
[verify a download](../installation.md#verify-a-download)). A tag can be moved,
so the clone alone proves nothing.

## 5. Sign-in

People sign in with an invite code and their computer's own key: device sign-in,
the only one since v1.0 ([sign-in methods](deployment.md#sign-in-methods)). The
server needs no Google OAuth client and no sign-in secret: leave `CLAWBRIDGE_AUTH`
unset (it is `device`). See [signing in with invites and device keys](../sign-in.md).

## 6. Install the server

Put the release on the server and write `/etc/clawbridge/clawbridge.env`
(root-owned, mode 0600) with the
[private configuration](deployment.md#private-configuration). It needs at least
`POSTGRES_PASSWORD`, `CLAWBRIDGE_DOMAIN` (the server's DNS name) and
`CLAWBRIDGE_BOOTSTRAP_OWNER` (your own email). Make the database password on the
server, before the first `compose up`, with `clawbridge cloud env init` and the route's
flags (`--vm clawbridge-1 --project … --zone …`, `--instance … --region …`, or
`--ssh HOST`): it prints only the key's name. Then start the stack with Compose.
Caddy obtains the TLS certificate for the domain.

**On AWS:** backups go to S3. Set them in the same file:
- `CLAWBRIDGE_BACKUP_URL=s3://BUCKET`;
- `CLAWBRIDGE_BACKUP_REGION=eu-west-1`, the bucket's region.

The backup timer uploads with the instance role (see
[backup and recovery](backup-and-recovery.md)).

**On your own server:** run `clawbridge cloud env init --ssh admin@203.0.113.7`
before the first `compose up`; it makes the database password on the host and
prints only its name. Then add the settings, including the backup store:
- `CLAWBRIDGE_BACKUP_URL=s3://BUCKET`;
- `CLAWBRIDGE_BACKUP_REGION`, the store's own region name (`auto` on R2);
- `CLAWBRIDGE_BACKUP_ENDPOINT=https://s3.eu-central-003.example.com`.

The store's key is yours to enter, with `cloud secret set
CLAWBRIDGE_BACKUP_ACCESS_KEY_ID --ssh admin@203.0.113.7` and the same for
`CLAWBRIDGE_BACKUP_SECRET_ACCESS_KEY`. The backup uploads in the pinned aws-cli
image, so the host needs no `aws` CLI.

Done when:

```sh
# Google Cloud
clawbridge cloud check infra server version --server https://clawbridge.example.com --vm clawbridge-1 --project example-project --zone europe-west1-b
# AWS
clawbridge cloud check infra server version --server https://clawbridge.example.com --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012
# Your own server, with the first backup run
clawbridge cloud check infra server version backups --server https://clawbridge.example.com --ssh admin@203.0.113.7
```

- `server` passes when HTTPS answers with a valid certificate and the server
  reports itself healthy. With a route to the server (`--vm`, `--instance` or
  `--ssh`) it also checks the env file's required settings, that the api, caddy
  and db containers run, and that the running server uses device sign-in.
- `version` passes when the server runs the same release as your client.

## 7. Create the workspace

The host operator lets the owner named in `CLAWBRIDGE_BOOTSTRAP_OWNER` in with a
one-time code, which also creates the workspace:
1. On the server host, in your own terminal (never through an agent), run
   `operator bootstrap-invite --workspace my-team --server https://clawbridge.example.com`
   inside the api container (`docker compose … run --rm --no-deps api operator …`;
   the full command is in the link below). It creates the workspace, owned
   by `CLAWBRIDGE_BOOTSTRAP_OWNER`, and prints a single-use code valid for 24 hours.
   The command is under
   [letting the owner in](deployment.md#letting-the-owner-in-operator-bootstrap-invite).
2. On your own computer: `clawbridge --server https://clawbridge.example.com join
   --email OWNER`, which asks for the code without showing it. The workspace has no
   project yet, so the terminal view opens on **Waiting for a project** and shows
   `clawbridge project create SLUG`; once you've created it, the view opens on it.

**On a server you reach over SSH:** one command on your own computer does both
steps. The code goes only on ssh's stdin. `clawbridge onboard`'s hand-over for
your own server offers to run it for you once the checks pass:

```sh
clawbridge cloud bootstrap --ssh admin@clawbridge.example.com \
  --server https://clawbridge.example.com --workspace my-team --email OWNER
```

**On Google Cloud:** one command on your own computer does both steps, over
your own `gcloud compute ssh` through IAP. The code goes only on that session's
stdin. `clawbridge onboard`'s Google Cloud hand-over offers to run it for you
once the checks pass:

```sh
clawbridge cloud bootstrap --vm clawbridge-1 --project PROJECT_ID --zone ZONE \
  --server https://clawbridge.example.com --workspace my-team --email OWNER
```

- **Connect once first:** ClawBridge runs gcloud with `--quiet`, so no prompt can
  read the code from stdin. So gcloud must already have your key
  (`~/.ssh/google_compute_engine`): run `gcloud compute ssh clawbridge-1 --project
  PROJECT_ID --zone ZONE --tunnel-through-iap` once in your own terminal, where it
  makes the key and can ask you for a passphrase. Without it, the command refuses
  before sending anything.
- **Your SSH key:** with OS Login on (`enable-oslogin=TRUE`), gcloud uses your OS
  Login profile. Without it, gcloud may add your key to the project's metadata,
  which every VM in the project trusts, or to the VM's own when the VM blocks
  project-wide keys. ClawBridge passes `--ssh-key-expire-after=1h`, so that key
  expires within the hour, and prints a one-line warning naming where it went.
  The warning reads the metadata only: if your organization enforces OS Login
  (`constraints/compute.requireOsLogin`), it is a false alarm. Prefer OS Login.
- **The host key:** the first connection trusts the VM's key as gcloud records it
  in `~/.ssh/google_compute_known_hosts` (trust on first use); later ones check it.
- **IAP's firewall rule:** IAP reaches the VM from 35.235.240.0/20 on tcp:22. If no
  rule allows that, the command says so instead of printing gcloud's error.

**On AWS:** the server host is reached over SSM, not SSH, so one command on your
own computer does both steps:

```sh
clawbridge cloud bootstrap --instance INSTANCE_ID --region REGION --profile PROFILE \
  --account ACCOUNT_ID --server https://clawbridge.example.com --workspace my-team --email OWNER
```

It makes the one-time code on your computer and hands it to the server's
`operator bootstrap-invite --code-stdin` through a short-lived encrypted
parameter, so it is in no SSM command history and never on screen, then joins
this device with it. Run it yourself, never through an agent. If you set the server up
with `clawbridge onboard` (on Google Cloud, AWS or your own server), it offers to run
this for you once the checks pass (or when you run `clawbridge onboard` again and
resume), and then goes on to the first project (section 8) and offers to open ClawBridge.

Done when `clawbridge cloud check workspace` passes: you are signed in, and you
own the workspace.

## 8. The first project

```sh
clawbridge project create my-project
```

With `clawbridge onboard`, this step is asked for you once the workspace exists:
after its own `cloud bootstrap`, or, when you signed in by hand, when you run
`clawbridge onboard` again and resume. It creates the
project, saves it as your profile's default and offers to open ClawBridge.

Optionally bind it to a Linear team with `clawbridge linear link` (see
[Linear](../linear.md)).

## 9. The Linear app

With the app, agents' Linear writes are made by the server as the ClawBridge
app rather than by anyone's personal account. In Linear, create an OAuth
application with the redirect URL `https://clawbridge.example.com/linear/callback`,
a page that only says the app is installed. Then enter its credentials from your
own terminal:

```sh
# Google Cloud
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_ID --vm clawbridge-1 --project example-project --zone europe-west1-b
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET --vm clawbridge-1 --project example-project --zone europe-west1-b
# AWS
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_ID --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012
```

On AWS, run one `secret set` at a time per key: two at once would share the
parameter's name.

Make the token key on the server with the same command as the database password, so it
is never printed and is added only when the file has none yet:

```sh
clawbridge cloud env init CLAWBRIDGE_LINEAR_APP_TOKEN_KEY --vm clawbridge-1 --project example-project --zone europe-west1-b
# or --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding, or --ssh admin@203.0.113.7
```

The entries and their rules are in
[the ClawBridge Linear app](deployment.md#the-clawbridge-linear-app-optional).
Recreate the API so it reads the new settings (`docker compose … up -d --no-deps api`,
as in the [deployment guide](deployment.md)), check it with `clawbridge cloud check
linear-app` and the route's flags, then install the app in your Linear workspace and add
it to each private team the project binds.

## 10. Invite your team

Invite each person to the project; they sign in with their own device key:

```sh
clawbridge invite --email colleague@example.com --grant my-project=member
```

Give them the invite link, or the code and the server's URL, the way you'd send a
password. They join with Start's "Join a team server" (see
[joining a project](../onboarding.md) and
[invites and the one-time code](../sign-in.md#invites-and-the-one-time-code)). For
someone who has already joined, `clawbridge project role-set colleague@example.com member`
changes their role in a project.

## 11. The dispatcher (optional)

The dispatcher is an optional coordinator that confirms routine claims and
escalates the rest to a person. It is off until the project owner turns it on (rules
mode); model mode is still being proven. See [the dispatcher](../dispatcher.md).

## Afterwards

After the first backup run, the backups check confirms a recent backup exists:

```sh
# Google Cloud
clawbridge cloud check backups --bucket example-project-clawbridge-backups --project example-project
# AWS
clawbridge cloud check backups --bucket clawbridge-backups-123456789012-eu-west-1 --region eu-west-1 --profile clawbridge-onboarding
```

Updates, backups, the restore drill and rollback are in the
[deployment guide](deployment.md) (on AWS, [updating a server on AWS](deployment.md#updating-a-server-on-aws))
and [backup and recovery](backup-and-recovery.md).
