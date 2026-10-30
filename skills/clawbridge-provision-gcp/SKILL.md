---
name: clawbridge-provision-gcp
description: "Provision a ClawBridge server on Google Cloud for the user, step by step: VM, static IP, data disk, firewall, backups, server install. Use when the user asks to set up their own ClawBridge server on Google Cloud, or when a ClawBridge onboarding TASK.md names this skill."
---

# Provision ClawBridge on Google Cloud

You are provisioning a ClawBridge server in the user's own Google Cloud project,
with their own `gcloud` credentials. The user approves each of your commands.
A step is done only when its `clawbridge cloud check` passes, never because a
command succeeded.

## Hard rules

Say these six rules to the user in your first message, then follow them.

1. **Explicit project.** Every `gcloud` command names `--project`, and
   `--region` or `--zone` where it takes one. Before anything else, check
   that the active account and project match what the user chose (step 1); if
   they differ, stop and ask.
2. **Cost first.** Before creating anything billable, show the itemised monthly
   estimate from [references/cost.md](references/cost.md) and get the user's
   explicit yes. One yes covers exactly the listed resources; anything added or
   changed needs a new yes.
3. **Secrets stay out of your hands.** Generate database passwords and keys
   **on the VM**, straight into the root-only env file, with commands that
   print nothing. When a step needs a secret the user holds (the Linear app's
   credentials), stop and ask the user to run
   `clawbridge cloud secret set KEY` in their own terminal. To check the env
   file, only count: `sudo grep -c '^KEY=.' /etc/clawbridge/clawbridge.env`.
4. **Ask before every delete.** Delete, stop, resize or replace a resource only
   with the user's explicit yes for that resource. Touch only resources this
   playbook created, and record each name you create in `created.txt` in your
   working directory as you go.
5. **Stop on mismatch.** Stop and ask when anything differs from this playbook:
   a name already exists, a quota is short, an API is disabled, or a check
   fails twice. Report what you saw; the user decides the next move.
6. **The check decides.** After each step, run its check and report the output
   verbatim.

## Inputs

Collect these from the user (or the onboarding `TASK.md`) before step 2. None is
a secret.

| Input | Example | Notes |
|---|---|---|
| Project | `example-project` | billing enabled |
| Region, zone | `europe-west1`, `europe-west1-b` | close to the team |
| Domain | `clawbridge.example.com` | DNS the user controls |
| Owner email | `you@example.com` | the person who becomes workspace owner |
| VM size | `e2-small` | from the cost table |
| Backup bucket | `example-project-clawbridge-backups` | globally unique |

## Steps

Run each step's commands from [references/commands.md](references/commands.md),
in order. `VM` below is `--vm clawbridge-1 --project PROJECT --zone ZONE`.

1. **Preflight.** Account, project, billing and APIs match. Done when the
   preflight commands show the chosen account and project and all three APIs
   enabled.
2. **Cost.** Show the estimate; record the yes. Done when the user said yes.
3. **Provision.** Service account, bucket, static IP, data disk, firewall, VM;
   then ask the user to create the DNS `A` record. Done when
   `clawbridge cloud check infra VM --server https://DOMAIN` fails only with
   `data_not_mounted` (step 4 mounts the disk).
4. **Prepare the VM.** Format and mount the data disk (new disk only), install
   Docker, check out the release. Done when the same infra check fails only
   with `port_closed` (nothing listens on 443 until step 6).
5. **Sign-in.** Read [references/signin.md](references/signin.md): device keys
   only, with no Google OAuth client (v1.0).
6. **Install the server.** Write the env file on the VM, start the stack, and
   install the disk guard and backup timer. Done when `clawbridge cloud check
   infra server version VM --server https://DOMAIN` passes.
7. **Hand back.** The user creates the workspace in their own terminal (the
   sign-in reference names the command). Done when `clawbridge cloud check
   workspace` passes. Then `clawbridge cloud check backups --bucket BUCKET
   --project PROJECT` passes after the first backup run.

When every check passes, give the user the list from `created.txt` and the
monthly estimate you showed, and stop.

## When a check fails

Show the check's reason and advice. Fix only what the advice names, within
these rules, and run the check again. If it fails a second time, stop (rule 5).
