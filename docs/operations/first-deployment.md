# First deployment

This guide takes a team from nothing to its own running ClawBridge server, a
first project and the Linear app. It is the written path behind Start's "Set up
my own server" (`clawbridge onboard`, choice 3), for people who do the steps
themselves rather than hand them to a coding agent.

It follows a fixed sequence of steps. A step is done when its check passes, never
when someone says it is. The checks are deterministic, read no secret values and
change nothing:

```sh
clawbridge cloud check server version     # after the server is installed
clawbridge cloud check workspace          # after setup --cloud
```

The exact commands for each cloud (Google Cloud first, then AWS and any VM with
Docker) arrive with the provisioning playbooks, together with their cost tables
and checks for the infrastructure, sign-in, the Linear app and backups. Until
then, this guide names what each step needs and links to the
[deployment guide](deployment.md) for the server itself. Examples use
`example.com`, `my-team` and a Google Cloud project named `example-project`.

## Rules that hold throughout

- **Name everything explicitly.** Every cloud command names the project, region
  and zone. Never rely on a CLI's default project.
- **Cost first.** Before creating anything billable, write down the itemised
  monthly estimate: the VM type, disk size, static IP, egress assumption and
  backup storage. The estimate is yours to approve; anything added later needs a
  new estimate.
- **Secrets are generated on the VM, or entered by you.** Database passwords and
  token keys are generated on the VM (`openssl rand`) straight into the
  root-only environment file and never printed. Secrets you hold, such as the
  Linear app's client secret, are entered with
  `clawbridge cloud secret set KEY`. It reads the value with hidden input and
  sends it over your own SSH session on stdin, so it never appears in a command
  line, a log or an agent's transcript.
- **Never print the environment file.** To check that a key is set, count it:
  `sudo grep -c '^KEY=.\+' /etc/clawbridge/clawbridge.env`.

## 1. Choose the cloud, project, region and domain

You need a cloud project you can bill, a region close to your team, and a domain
or subdomain whose DNS you control (for example `clawbridge.example.com`).

## 2. Estimate the cost

A small VM is enough for a team: about $15–30 a month with its disk, static IP
and backups. The playbooks carry a fixed estimate table per VM size and cloud.

## 3. Provision

Create, with the names recorded as you go:

- a VM with Docker and the Compose plugin;
- a static IP attached to it, and a DNS `A` record for the domain pointing at it;
- a firewall rule opening only TCP 443 to the VM (Caddy obtains its certificate
  over TLS on 443 and serves no plain HTTP; see
  [HTTPS and operator access](deployment.md#https-and-operator-access)), and an
  administrative path for SSH (on Google Cloud, IAP);
- a separate persistent data disk for the database, mounted at
  `/var/lib/clawbridge-postgres` (see
  [persistent disk and backup guards](deployment.md#persistent-disk-and-backup-guards));
- a private backup bucket, and an identity for the VM that may write to it (see
  [backup and recovery](backup-and-recovery.md)).

## 4. Sign-in clients

Until the device sign-in release, the server verifies Google sign-in. Create the
Google OAuth consent screen and two OAuth clients, one Desktop app and one TVs
and Limited Input device, as described under
[Google identity](deployment.md#google-identity) and
[private configuration](deployment.md#private-configuration).

## 5. Install the server

Put the release on the VM, write `/etc/clawbridge/clawbridge.env` (root-owned,
mode 0600) with the [private configuration](deployment.md#private-configuration),
generating the database password on the VM, and start the stack with Compose.
Caddy obtains the TLS certificate for the domain.

Done when:

```sh
clawbridge cloud check server version --server https://clawbridge.example.com
```

`server` passes when HTTPS answers with a valid certificate and the server
reports itself healthy. `version` passes when the server runs the same release as
your client.

## 6. Create the workspace

The identity named in `CLAWBRIDGE_BOOTSTRAP_OWNER` signs in and initializes the
workspace once:

```sh
clawbridge --server https://clawbridge.example.com --audience AUDIENCE setup --cloud --name my-team
```

Done when `clawbridge cloud check workspace` passes: you are signed in, and you
own the workspace.

## 7. The first project

```sh
clawbridge project create my-project
```

Optionally bind it to a Linear team with `clawbridge linear link` (see
[Linear](../linear.md)).

## 8. The Linear app

With the app, agents' Linear writes are made by the server as the ClawBridge
app rather than by anyone's personal account. In Linear, create an OAuth
application with the redirect URL `https://clawbridge.example.com/linear/callback`,
a page that only says the app is installed. Then enter its credentials from your
own terminal:

```sh
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_ID --vm clawbridge-1 --project example-project --zone europe-west1-b
clawbridge cloud secret set CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET --vm clawbridge-1 --project example-project --zone europe-west1-b
```

Generate the token key on the VM, in its shell, so it is never printed. It is
added only when the file has no token key yet, so it is set exactly once:

```sh
[ "$(sudo grep -c '^CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=' /etc/clawbridge/clawbridge.env)" = 0 ] &&
  openssl rand -base64 32 | sed 's/^/CLAWBRIDGE_LINEAR_APP_TOKEN_KEY=/' |
  sudo tee -a /etc/clawbridge/clawbridge.env >/dev/null
```

On a VM outside Google Cloud, use `--ssh HOST` instead of `--vm`, `--project`
and `--zone`. The entries and their rules are in
[the ClawBridge Linear app](deployment.md#the-clawbridge-linear-app-optional).
Restart the API, install the app in your Linear workspace, and add it to each
private team the project binds.

## 9. Invite your team

Grant each person a role in the project:

```sh
clawbridge project role-set colleague@example.com member
```

They join with Start's "Join a team server" (see [joining a project](../onboarding.md)).

## 10. The dispatcher (optional)

The dispatcher is an optional coordinator that confirms routine claims and
escalates the rest to a person. It is being built in steps and is off; see
[the dispatcher](../dispatcher.md).

## Afterwards

Updates, backups and rollback are in the [deployment guide](deployment.md) and
[backup and recovery](backup-and-recovery.md).
