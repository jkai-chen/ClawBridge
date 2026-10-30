---
name: clawbridge-provision-vm
description: "Install a ClawBridge server on a Linux host the user already has (a VPS, a bare-metal box, a home server with a public address), reached over the user's own SSH with passwordless sudo: data volume, firewall, release checkout, env file, Docker Compose stack, backups to S3-compatible storage off the host. Use when the user asks to set up ClawBridge on their own server or any VM with Docker, or when a ClawBridge onboarding TASK.md names this skill."
---

# Install ClawBridge on a server you already have

You are installing a ClawBridge server on a Linux host the user already owns,
over **their own SSH**, as a user with passwordless sudo. The user approves each
of your commands. A step is done only when its `clawbridge cloud check` passes,
never because a command succeeded.

This is someone's own machine: other services, users and files may live on it.
You run commands there as root, so you **could** read the server's env file
and its secrets. Rule 3 forbids it, and the user's approval of each command is
what enforces it.

## Hard rules

Say these six rules to the user in your first message, then follow them.

1. **Explicit host.** Every remote command goes to the host the user chose, as
   `ssh HOST` (the exact options are in
   [references/commands.md](references/commands.md)). Before anything else, show
   its `hostname` and `uname -sm` and get the user's yes that it is the right
   machine; if either changes later, stop and ask.
2. **Cost first.** There is no VM to buy. Before creating the backup bucket,
   show the estimate from [references/cost.md](references/cost.md) for the
   user's store and get their explicit yes.
3. **Secrets stay out of your hands.** The generated secrets come from
   `clawbridge cloud env init --ssh HOST`, which makes them on the host and
   prints key names only. When a step needs a secret the user holds (the backup
   store's key, the Linear app's credentials), stop and ask the user to run
   `clawbridge cloud secret set KEY --ssh HOST` in their own terminal. Never
   print, copy, `cat`, `grep` a value from or edit
   `/etc/clawbridge/clawbridge.env` by hand; to check it, only count:
   `sudo grep -c '^KEY=.' /etc/clawbridge/clawbridge.env`.
4. **Ask before every delete.** Delete, stop, overwrite, format or reconfigure
   something only with the user's explicit yes for that thing. Never touch
   containers, volumes, users, firewall rules or files this playbook didn't
   create. Record each thing you create in `created.txt` in your working
   directory as you go.
5. **Stop on mismatch.** Stop and ask when anything differs from this playbook:
   the host isn't the one confirmed, port 443 is already in use, Docker or
   Compose is missing or too old, a device holds data you didn't create, the
   release commit differs, or a check fails twice. Report what you saw; the
   user decides the next move.
6. **The check decides.** After each step, run its check and report the output
   verbatim, including every `warn` line.

**Out of scope:** installing Docker, upgrading the OS, and changing SSH or sudo
configuration. When the preflight names one of these, give the user its advice
and the vendor's link, and wait for them to fix it.

## Inputs

Collect these from the user (or the onboarding `TASK.md`) before step 2. None is
a secret.

| Input | Example | Notes |
|---|---|---|
| SSH host | `admin@203.0.113.7`, or an alias from `~/.ssh/config` | a user with key-based SSH and passwordless sudo |
| Domain | `clawbridge.example.com` | DNS the user controls, with an `A` (or `AAAA`) record for the host |
| Owner email | `you@example.com` | the person who becomes workspace owner |
| Backup endpoint | `https://s3.eu-central-003.backblazeb2.com` | the store's S3 endpoint, `https://HOST[:PORT]` only |
| Backup bucket | `clawbridge-backups-example` | private |
| Backup region | `eu-central-003` | the store's own region name (`auto` on R2) |

## Steps

Run each step's commands from [references/commands.md](references/commands.md),
in order. `TARGET` below is `--ssh HOST --server https://DOMAIN`.

1. **Preflight.** Confirm the host (rule 1), then run `clawbridge cloud check
   infra TARGET`. Done when the user said the host is right and the check fails
   only with `data_not_mounted` or `port_closed` (or warns). `docker_missing`,
   `compose_too_old` or `unsupported_platform` is out of scope: stop with its
   advice.
2. **Cost.** Show the backup store's estimate; record the yes. The user creates
   the bucket and its put-and-list key as the table in commands.md says. Done
   when the user said yes and confirmed the key's scope.
3. **Prepare the host.** The data volume (or the user's stated opt-out), the
   firewall, and the release checkout checked against its attested commit.
   Done when `clawbridge cloud check infra TARGET` fails only with
   `port_closed`.
4. **The env file.** `cloud env init`, the fixed settings, and the user's
   `cloud secret set` for the backup key. Done when the counts in commands.md
   print `1` for each key.
5. **Sign-in.** Read [references/signin.md](references/signin.md).
6. **Install the server.** The disk guard and backup timer, the stack, the first
   backup. Done when `clawbridge cloud check infra server version backups
   TARGET` passes (a `warn` passes; report it).
7. **Hand back.** The user creates the workspace in their own terminal (the
   sign-in reference names the command). Done when `clawbridge cloud check
   workspace` passes.

When every check passes, give the user the list from `created.txt`, the
estimate you showed, and every `warn` line the checks printed, and stop.

## When a check fails

Show the check's reason and advice. Fix only what the advice names, within
these rules, and run the check again. If it fails a second time, stop (rule 5).
