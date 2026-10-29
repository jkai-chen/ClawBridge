---
name: clawbridge-provision-local
description: "Run a ClawBridge server on the user's own machine, for them alone or for their team on the LAN, with Docker and deploy/local-host.sh: no cloud, no cost. Use when the user asks to host ClawBridge on this machine or their LAN, or when a ClawBridge onboarding TASK.md names this skill."
---

# Run ClawBridge on this machine

You are starting a ClawBridge server on the user's own machine with Docker and
the release's `deploy/local-host.sh`. It runs in one of two shapes:

- **solo:** for the user alone, at `http://127.0.0.1:8443`;
- **lan:** for their team on the local network, at `https://HOST`, with a
  certificate from the server's own internal certificate authority.

Nothing is created in any cloud and nothing costs money. The user approves each
of your commands. A step is done only when its check passes, never because a
command succeeded.

## Hard rules

Say these six rules to the user in your first message, then follow them.

1. **No installs without asking.** Install nothing yourself. If Docker or
   `docker compose` v2 is missing, stop and tell the user what to install:
   Docker Engine 28 or newer on Linux, or OrbStack or Docker Desktop on macOS.
2. **Never use sudo.** Every command runs as the user. If one needs root, stop
   and tell the user why.
3. **The user's things stay theirs.** Never run `local-host.sh trust`,
   `invite-owner` or `uninstall`. `trust` exports the server's root
   certificate (Caddy's) and prints the commands that trust it; the user runs
   those, and they change this machine's trust store. The sign-in code is a
   secret for the user's own terminal, and uninstalling deletes data. Run `up`
   exactly as written, with `</dev/null`, and never in an interactive
   terminal: without a terminal the script never makes the sign-in code, so
   it can't reach you. Never stop, remove or change other containers, volumes,
   networks, images or Compose projects.
4. **Secrets stay out of your hands.** The script generates the server's
   secrets into `~/.clawbridge-server/clawbridge.env`. Never read, print, copy
   or edit that file. To check a setting in it, only count:
   `grep -c '^KEY=.' ~/.clawbridge-server/clawbridge.env`.
5. **Stop on mismatch.** Stop and ask when anything differs from this playbook:
   a port is already in use, a different setup already exists (`up` refuses
   it), or a check fails twice. Report what you saw; the user decides the next
   move.
6. **The check decides.** After each step, run its check and report the output
   verbatim.

## Inputs

They come from the onboarding `TASK.md`, or from the user. None is a secret.

| Input | Example | Notes |
|---|---|---|
| Shape | `solo` or `lan` | who uses the server |
| Owner email | `owner@example.test` | becomes the workspace owner |
| Workspace | `my-team` | lowercase letters, digits, `_` and `-` |
| LAN host (lan only) | `clawbridge.lan` | a DNS name or IPv4 address the team's machines reach |
| LAN address (lan only) | `192.0.2.10` | this machine's IPv4 address on the LAN |
| ClawBridge version | `v0.3.10` | the user's `clawbridge version` |

## Steps

Work in the directory `TASK.md` is in; the source goes in `./clawbridge`.

1. **Docker.** Run:

   ```sh
   docker version
   docker compose version
   ```

   Done when both answer, the server side included, and Compose is v2. If
   either is missing or the daemon is not running, stop (rule 1).
2. **The source at this release.** With the version `vX.Y.Z` from `TASK.md`:

   ```sh
   git clone --depth 1 --branch vX.Y.Z https://github.com/jkai-chen/ClawBridge clawbridge
   ```

   On a re-run, reuse an existing `./clawbridge` when
   `git -C clawbridge describe --tags --exact-match` prints the same tag; if it
   names another tag or has changes, stop (rule 5). On a development build
   (the version is not `vX.Y.Z`), there is no tag: stop and ask the user for a
   release tag, or for the path of a checkout to use. Done when
   `clawbridge/deploy/local-host.sh` exists at that tag.
3. **Start the server.** Run the `up` command from `TASK.md` exactly, for
   example:

   ```sh
   sh clawbridge/deploy/local-host.sh up --owner owner@example.test --workspace my-team </dev/null
   sh clawbridge/deploy/local-host.sh up --lan clawbridge.lan --bind 192.0.2.10 --owner owner@example.test --workspace my-team </dev/null
   ```

   The first run builds the server image, which takes minutes. Run without a
   terminal, `up` makes no sign-in code and says to run `invite-owner`: that is
   expected and is the user's step, not yours (rule 3). If it refuses because
   a port is in use or a different setup exists, stop (rule 5). Done when it
   exits with success.
4. **Check, then stop.** Run:

   ```sh
   clawbridge cloud check server version --local --server http://127.0.0.1:8443
   ```

   The server answers on `127.0.0.1:8443` in both shapes, so this is the
   check that decides. Done when `server` and `version` pass (a development
   build's `version` warns). In the lan shape, then run:

   ```sh
   clawbridge cloud check trust --local
   ```

   It is expected to fail with `untrusted_ca` until the user trusts the
   server's root certificate, or with `dns` if this machine doesn't resolve
   the LAN host. Report both outputs verbatim, say so, and fix neither: those
   are the user's sign-in steps. Then stop: the user does the sign-in in
   [references/signin.md](references/signin.md) in their own terminal.

## When a check fails

Show the check's reason and advice. Fix only what the advice names, within
these rules, and run the check again. If it fails a second time, stop (rule 5).
`deploy/local-host.sh status` shows the server's containers; it prints no
secret.
