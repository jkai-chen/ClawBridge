---
name: clawbridge-provision-aws
description: "Provision a ClawBridge server on AWS for the user, step by step: EC2 instance reached over SSM (no SSH), Elastic IP, encrypted data volume, HTTPS-only security group, S3 backups, server install. Use when the user asks to set up their own ClawBridge server on AWS, or when a ClawBridge onboarding TASK.md names this skill."
---

# Provision ClawBridge on AWS

You are provisioning a ClawBridge server in the user's own AWS account, with
their own AWS CLI profile. The user approves each of your commands. A step is
done only when its `clawbridge cloud check` passes, never because a command
succeeded.

The instance has no SSH: no key pair and no port 22. Everything you run on it
goes through `aws ssm send-command`, which records each command and its output
in the account's SSM history. That is why rule 3 keeps every secret out of
those commands.

## Hard rules

Say these six rules to the user in your first message, then follow them.

1. **Explicit profile and region.** Every `aws` command names `--profile
   PROFILE` and `--region REGION`. Before anything else, check that
   `aws sts get-caller-identity` shows the account the user chose (step 1); if
   it differs, stop and ask.
2. **Cost first.** Before creating anything billable, show the itemised monthly
   estimate from [references/cost.md](references/cost.md) and get the user's
   explicit yes. One yes covers exactly the listed resources; anything added or
   changed needs a new yes.
3. **Secrets stay out of your hands.** Generate database passwords and keys
   **on the instance**, straight into the root-only env file, with commands
   that print nothing; never put a secret's value in a command you send.
   When a step needs a secret the user holds (the Linear app's
   credentials), stop and ask the user to run
   `clawbridge cloud secret set KEY` in their own terminal. To check the env
   file, only count: `grep -c '^KEY=.' /etc/clawbridge/clawbridge.env`.
4. **Ask before every delete.** Delete, stop, resize or replace a resource only
   with the user's explicit yes for that resource. Touch only resources this
   playbook created, and record each name or ID you create in `created.txt` in
   your working directory as you go.
5. **Stop on mismatch.** Stop and ask when anything differs from this playbook:
   a name already exists, a quota is short, a service is not available in the
   region, or a check fails twice. Report what you saw; the user decides the
   next move.
6. **The check decides.** After each step, run its check and report the output
   verbatim.

## Inputs

Collect these from the user (or the onboarding `TASK.md`) before step 2. None is
a secret.

| Input | Example | Notes |
|---|---|---|
| Profile | `clawbridge-onboarding` | a named AWS CLI profile the user set up; you never create or edit it |
| Account ID | `123456789012` | the profile must be signed in to it |
| Region, zone | `eu-west-1`, `eu-west-1a` | close to the team |
| Domain | `clawbridge.example.com` | DNS the user controls |
| Owner email | `you@example.com` | the person who becomes workspace owner |
| Instance size | `t4g.small` | from the cost table |
| Backup bucket | `clawbridge-backups-123456789012-eu-west-1` | globally unique |

## Steps

Run each step's commands from [references/commands.md](references/commands.md),
in order. `TARGET` below is `--instance INSTANCE_ID --region REGION --profile
PROFILE --account ACCOUNT_ID`, where `INSTANCE_ID` is the ID step 3 records.

1. **Preflight.** Account, region and zone match, and none of the names exist.
   Done when the preflight commands show the chosen account and print no
   existing resource.
2. **Cost.** Show the estimate; record the yes. Done when the user said yes.
3. **Provision.** Network, security group, bucket, role, data volume,
   instance, Elastic IP; then the user creates the DNS `A` record. Done when
   `clawbridge cloud check infra TARGET --server https://DOMAIN` fails only
   with `data_not_mounted` (step 4 mounts the volume). Right after the
   instance starts, `ssm_offline` is normal for a few minutes: the instance has
   no public IP, so it can't reach SSM until the Elastic IP is associated. Wait
   and run the check again before counting it as a failure.
4. **Prepare the instance.** Format and mount the data volume (new volume
   only), install Docker, check out the release. Done when the same infra check
   fails only with `port_closed` (nothing listens on 443 until step 6).
5. **Sign-in.** Read [references/signin.md](references/signin.md): device keys
   only, with no Google OAuth client (v1.0).
6. **Install the server.** Write the env file on the instance, start the stack,
   and install the disk guard and backup timer. Done when `clawbridge cloud
   check infra server version TARGET --server https://DOMAIN` passes.
7. **Hand back.** The user creates the workspace in their own terminal (the
   sign-in reference names the command). Done when `clawbridge cloud check
   workspace` passes. Then `clawbridge cloud check backups --bucket BUCKET
   --region REGION --profile PROFILE` passes after the first backup run.

When every check passes, give the user the list from `created.txt` and the
monthly estimate you showed, and stop.

## When a check fails

Show the check's reason and advice. Fix only what the advice names, within
these rules, and run the check again. If it fails a second time, stop (rule 5).
