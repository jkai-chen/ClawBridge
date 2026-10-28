# First deployment

This guide takes a team from nothing to its own running ClawBridge server, a
first project and the Linear app, on **Google Cloud** or **AWS**. It is the
written path behind Start's "Set up my own server" (`clawbridge onboard`, choice
3, which then asks which cloud), for people who do the steps themselves rather
than hand them to a coding agent.

It follows a fixed sequence of steps. A step is done when its check passes, never
when someone says it is. The checks are deterministic, read no secret values and
change nothing:

```sh
clawbridge cloud check infra              # after the server is provisioned
clawbridge cloud check server version     # after the server is installed
clawbridge cloud check workspace          # after setup --cloud
clawbridge cloud check backups            # after the first backup run
```

The exact commands for each cloud are in its provisioning playbook, with its
cost table and the checks for each step:
- Google Cloud: `clawbridge skill install --playbook gcp`. The server is a
  Compute Engine VM reached over IAP, and its backups go to Cloud Storage.
- AWS: `clawbridge skill install --playbook aws`. The server is an EC2 instance
  reached over SSM, with no SSH, and its backups go to S3.

A playbook for any VM with Docker comes later. This guide names what each step
needs and links to the [deployment guide](deployment.md) for the server itself.

Examples use `example.com` and `my-team`, and:
- **Google Cloud:** a project named `example-project`, in zone `europe-west1-b`;
- **AWS:** account `123456789012`, region `eu-west-1`, an AWS CLI profile named
  `clawbridge-onboarding` and an instance `i-0123456789abcdef0`.

## Which cloud

Both run the same server. The table shows what differs.

| | Google Cloud | AWS |
|---|---|---|
| Server | a Compute Engine VM | an EC2 instance (Amazon Linux 2023, arm64) |
| Your admin access | SSH through IAP, as your Google account | SSM, as your named AWS CLI profile, with no SSH port at all |
| Your CLI | `gcloud`, with a named configuration | AWS CLI v2, with a named profile you set up (`aws configure sso --profile clawbridge-onboarding`) |
| Backups | a Cloud Storage bucket | an S3 bucket |
| Where you run it | wherever `gcloud` runs | macOS, Linux or WSL (the AWS route reads its requests from `/dev/stdin`); on Windows, [set up WSL first](../installation.md#windows-set-up-wsl-for-the-aws-route) |
| Monthly estimate | about $15–30 | about $19 (`t4g.small`) or $31.50 (`t4g.medium`) |

ClawBridge never creates or edits your CLI profile or configuration, and never
reads `~/.aws` or the gcloud credential store.

## Rules that hold throughout

- **Name everything explicitly.** Every cloud command names the project, region
  and zone on Google Cloud, and `--profile` and `--region` on AWS. Never rely on
  a CLI's default project, profile or region.
- **Cost first.** Before creating anything billable, write down the itemised
  monthly estimate: the VM type, disk size, static IP, egress assumption and
  backup storage. The estimate is yours to approve; anything added later needs a
  new estimate.
- **Secrets are generated on the server, or entered by you.** Database passwords
  and token keys are generated on the server (`openssl rand`) straight into the
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

## 2. Estimate the cost

A small server is enough for a team. The playbooks carry a fixed estimate table
per size and cloud.
- **Google Cloud:** about $15–30 a month with its disk, static IP and backups.
- **AWS (`us-east-1`):** `t4g.small` is about $19 a month and `t4g.medium` about
  $31.50, with the encrypted data volume, Elastic IP and S3 backups. Other
  regions cost up to about 25% more. The Elastic IP is billed while allocated,
  even with the instance stopped.

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

Done when the infra check fails only with `data_not_mounted`. The next step
mounts the volume.

```sh
# Google Cloud
clawbridge cloud check infra --vm clawbridge-1 --project example-project --zone europe-west1-b --server https://clawbridge.example.com
# AWS
clawbridge cloud check infra --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012 --server https://clawbridge.example.com
```

On AWS, `ssm_offline` is normal for a few minutes right after the instance
starts: it has no public IP until the Elastic IP is associated, so it can't
reach SSM. Wait and run the check again before counting it as a failure.

## 4. Prepare the server

Format and mount the data volume (a new volume only), install Docker and the
Compose plugin, and check out the release. Done when the same infra check fails
only with `port_closed`: nothing listens on 443 until the server is installed.

## 5. Sign-in

Choose how people sign in. `CLAWBRIDGE_AUTH` in the server's env file sets it
([sign-in methods](deployment.md#sign-in-methods)).
- **Google sign-in** (the default, `google`): the server verifies Google sign-in.
  Create the Google OAuth consent screen and two OAuth clients, one Desktop app and
  one TVs and Limited Input device. They are described under
  [Google identity](deployment.md#google-identity) and
  [private configuration](deployment.md#private-configuration).
- **Device sign-in** (`device`, or `google,device` for both): people join with an
  invite code and their computer's own key, and a device-only server needs no Google
  OAuth client.
  - **What is released:** the server side shipped in v0.3.4, and the client
    commands that join and invite (`clawbridge join`, `invite`) in v0.3.5.
  - See [signing in with invites and device keys](../sign-in.md).

**On AWS:** Google sign-in needs those clients in a Google Cloud project you own,
even though the server runs on AWS. Their two client secrets go to the server
with `cloud secret set`, as in step 9.

## 6. Install the server

Put the release on the server and write `/etc/clawbridge/clawbridge.env`
(root-owned, mode 0600) with the
[private configuration](deployment.md#private-configuration), generating the
database password on the server. Then start the stack with Compose. Caddy
obtains the TLS certificate for the domain.

**On AWS:** backups go to S3. Set them in the same file:
- `CLAWBRIDGE_BACKUP_URL=s3://BUCKET`;
- `CLAWBRIDGE_BACKUP_REGION=eu-west-1`, the bucket's region.

The backup timer uploads with the instance role (see
[backup and recovery](backup-and-recovery.md)).

Done when:

```sh
# Google Cloud
clawbridge cloud check infra server version --server https://clawbridge.example.com --vm clawbridge-1 --project example-project --zone europe-west1-b
# AWS
clawbridge cloud check infra server version --server https://clawbridge.example.com --instance i-0123456789abcdef0 --region eu-west-1 --profile clawbridge-onboarding --account 123456789012
```

- `server` passes when HTTPS answers with a valid certificate and the server
  reports itself healthy.
- `version` passes when the server runs the same release as your client.

## 7. Create the workspace

The identity named in `CLAWBRIDGE_BOOTSTRAP_OWNER` signs in and initializes the
workspace once:

```sh
clawbridge --server https://clawbridge.example.com --audience AUDIENCE setup --cloud --name my-team
```

**With device sign-in** (device-only, so no Google sign-in to run `setup --cloud`
with): the host operator lets the owner in with a one-time code instead.
1. On the server host, in your own terminal (never through an agent), run
   `operator bootstrap-invite --workspace my-team`. It creates the workspace, owned
   by `CLAWBRIDGE_BOOTSTRAP_OWNER`, and prints a single-use code valid for 24 hours.
   The command is under
   [letting the owner in without Google](deployment.md#letting-the-owner-in-without-google-operator-bootstrap-invite).
2. On your own computer: `clawbridge --server https://clawbridge.example.com join
   --email OWNER`, which asks for the code without showing it. The workspace has no
   project yet, so the terminal view opens on **Waiting for a project** and shows
   `clawbridge project create SLUG`; once you've created it, the view opens on it.

`operator bootstrap-invite` shipped in v0.3.4, and `clawbridge join` in v0.3.5.

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
with `clawbridge onboard`'s AWS hand-over, it offers to run this for you once the
checks pass (or when you run `clawbridge onboard` again and resume), and then goes on
to the first project (section 8) and offers to open ClawBridge. It comes with the
next release (it is on the main branch, not in v0.3.4), and needs the server on
that release too; until then, use Google sign-in on AWS.

Done when `clawbridge cloud check workspace` passes: you are signed in, and you
own the workspace.

## 8. The first project

```sh
clawbridge project create my-project
```

With `clawbridge onboard`, this step is asked for you once the workspace exists:
after its own `cloud bootstrap` on AWS, or, when you signed in by hand (as on
Google Cloud), when you run `clawbridge onboard` again and resume. It creates the
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

Generate the token key on the server, in its shell, so it is never printed. It
is added only when the file has no token key yet, so it is set exactly once:

```sh
[ "$(sudo grep -c '^CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=' /etc/clawbridge/clawbridge.env)" = 0 ] &&
  openssl rand -base64 32 | sed 's/^/CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=/' |
  sudo tee -a /etc/clawbridge/clawbridge.env >/dev/null
```

The server's shell:
- **On Google Cloud:** `gcloud compute ssh` through IAP.
- **On AWS:** a Session Manager session, `aws ssm start-session --target
  i-0123456789abcdef0 --profile clawbridge-onboarding --region eu-west-1` (it
  needs AWS's Session Manager plugin). The command above prints nothing, so
  nothing lands in a session log.
- **On a server outside both clouds:** use `--ssh HOST` with `secret set`
  instead of the cloud flags.

The entries and their rules are in
[the ClawBridge Linear app](deployment.md#the-clawbridge-linear-app-optional).
Restart the API, install the app in your Linear workspace, and add it to each
private team the project binds.

## 10. Invite your team

Grant each person a role in the project:

```sh
clawbridge project role-set colleague@example.com member
```

They join with Start's "Join a team server" (see [joining a project](../onboarding.md)).

**With device sign-in**, invite them instead, and give each the code and the
server's URL yourself (the client commands are in v0.3.5):

```sh
clawbridge invite --email colleague@example.com --grant my-project=member
```

See [if your server uses invites](../onboarding.md#if-your-server-uses-invites-device-sign-in).

## 11. The dispatcher (optional)

The dispatcher is an optional coordinator that confirms routine claims and
escalates the rest to a person. It is being built in steps and is off; see
[the dispatcher](../dispatcher.md).

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
