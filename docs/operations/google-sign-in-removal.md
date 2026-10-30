# Removing Google sign-in (v1.0)

ClawBridge v1.0 signs people in with **device keys only**. Google sign-in, the OAuth flow, its settings and the
`Bearer` Google ID token are gone. This page is the order to switch a server that still accepts Google
(`CLAWBRIDGE_AUTH=google` or `google,device`) without locking anyone out.

What stays: a scheduler's Cloud Scheduler OIDC token (`/v1/tick`) and the Google Cloud onboarding
(`clawbridge cloud …` with gcloud) are not sign-in and are unchanged.

What a v1.0 server does with Google:
- **At start**, a server whose `CLAWBRIDGE_AUTH` (or `serve --auth`) names `google` refuses to start, printing one
  line and exiting with code 2:
  `clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)`
- **A `serve` command line** that still passes `--audience` (or an OAuth client flag) with a value refuses to start the
  same way, with exit code 2 (an empty one is ignored): `clawbridge serve: --audience was for Google sign-in, removed in v1.0
  (docs/operations/google-sign-in-removal.md#before-you-switch)`. Compose's own `serve` line no longer has them; a
  custom one (your own unit file or container command) must drop them.
- **A request signed with a Google token** is refused (`unauthenticated`): "Google sign-in was removed; this server
  accepts device keys only: ask an owner for an invite (or a recovery invite) and run clawbridge join; see
  docs/operations/google-sign-in-removal.md". (`device enroll` needs a Google sign-in, so it can't help here.)

There is no transition window: a v1.0 server never accepts a Google token. So every member moves to a device key
**before** the server switches, on the version you run today.

## Before you switch

Do all of this on your current (pre-v1.0) server, with `CLAWBRIDGE_AUTH=google,device`. If it says `google` only,
set `google,device` first, recreate the `api` container, and check:

```sh
curl -s https://clawbridge.example.com/v1/config     # shows "auth_methods":["google","device"]
```

1. **Every member's client is current.** Each member runs `clawbridge self-update` (or reinstalls). Device sign-in
   (`join`, `device enroll`) is in every client from v0.3.5; the invite link and `device add` from v0.3.6.
2. **Every member has an active device.** A member who signs in with Google runs, in their own terminal:

   ```sh
   clawbridge device enroll
   ```

   New people join with an invite (`clawbridge join`; see [sign-in](../sign-in.md#joining-a-device)). CI uses its
   own CI device (`clawbridge device add --ci NAME --grant PROJECT=member --expires 30d`), never a person's Google
   sign-in.
3. **The owner checks the list.** As the main owner, on a device-signed profile:

   ```sh
   clawbridge user list                              # each member, role and devices
   clawbridge device list --member ada@example.com   # one member's devices; none dormant
   ```

   Every **active** member needs at least one device that is live and not dormant. You, the main owner, need one too:
   ideally two (a second computer, from `clawbridge device add`), so a lost laptop never needs the host operator.
4. **Each member proves it.** Each member runs, on the device they will keep using:

   ```sh
   clawbridge --project PROJECT whoami               # shows "identity_provenance":"device-key"
   ```

   and tells the owner it said `device-key`. A member whose `whoami` says anything else is not ready: back to step 2.
   Write the list down (member, device label, `device-key` seen, date): it is the gate for the next section.
5. **Agents too.** Each agent session's MCP entry signs with its person's profile, so it moves with them. After a
   member enrols, a session's `whoami` (or `clawbridge doctor` with the session's `--mcp-*` flags, see
   [usage](../usage.md)) shows `device-key`.

**The gate:** do not continue until every active member, the main owner included, has a device-signed `whoami`
from step 4. A member you can't reach is either left out on purpose (they recover later with a recovery invite,
below) or revoked (`clawbridge user revoke EMAIL --confirm`).

## Switch the server to device keys

Still on your current version:

1. Set `CLAWBRIDGE_AUTH=device` in the server's environment file (`/etc/clawbridge/clawbridge.env` on a VM; the
   Secret Manager or Parameter Store value on GCP or AWS, see the [deployment guide](deployment.md)).
2. Recreate the `api` container so it reads the file.
3. Check:

   ```sh
   curl -s https://clawbridge.example.com/v1/config  # shows "auth_methods":["device"]
   clawbridge cloud check server --ssh HOST          # auth_mismatch until the running server matches
   clawbridge --project PROJECT whoami               # still works: device-key
   ```

4. Watch for a day if you can: anyone still on Google now gets "a ClawBridge-Device signature is required; this
   server accepts device keys only". **Rollback while still on the old version:** set `CLAWBRIDGE_AUTH` back to
   `google,device` and recreate `api`. Nothing stored changes, and Google works again at once.

## Deploy v1.0

1. **Update the deployment files (recommended).** v1.0's `deploy/compose.yaml` no longer passes `--audience` or the
   OAuth flags. An older one passes them empty, which v1.0 treats as absent, so it still starts; only a non-empty
   value is refused ("--audience was for Google sign-in, removed in v1.0"). Still, before recreating `api`, put the
   v1.0 release's source in `/opt/clawbridge/repo`, exactly as the [deployment guide](deployment.md)'s source-archive
   steps do (export the v1.0 commit, extract into a new directory, move the old one aside as `repo-recovery-STAMP`,
   move the new one into place), so the files match the release. This applies to the published-image path too: its
   `compose.yaml` and `compose.image.yaml` come from that directory. Keep `repo-recovery-STAMP` for a rollback. If
   you keep the older files, the env file must say `CLAWBRIDGE_AUTH=device` (the switch above), because they default
   it to `google`, which v1.0 refuses.
2. Deploy v1.0 as usual ([deployment guide](deployment.md): drain, swap, check). With
   `CLAWBRIDGE_AUTH=device` it starts; with `google` anywhere in it, the new container exits at once with the
   message above (nothing is served): set `device` and recreate it.
3. Check `/v1/config` again (it shows `"auth_methods":["device"]`) and a device-signed `whoami`.
4. Remove the Google sign-in settings from the environment file, keeping the file's mode and owner:
   `CLAWBRIDGE_AUDIENCE`, `CLAWBRIDGE_OAUTH_CLIENT_ID`, `CLAWBRIDGE_OAUTH_CLIENT_SECRET`,
   `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID` and `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET` (and from your secret store). The
   scheduler's `CLAWBRIDGE_TICK_AUDIENCE` and `CLAWBRIDGE_TICK_SERVICE_ACCOUNT` stay. v1.0 ignores them, and `clawbridge cloud
   check server` warns (rather than fails) while any is still in the env file. v1.0's Compose files no longer pass
   them to the server; a server that still gets them in its environment (older Compose files, or a server you start
   some other way) logs one line, "Google sign-in settings are set but unused since v1.0
   (CLAWBRIDGE_AUDIENCE, CLAWBRIDGE_OAUTH_*); remove them from the env file", with no values. After removing them,
   `cloud check server` shows no warning.
5. Optional, when you're sure: delete the OAuth client in the Google Cloud console (APIs & Services → Credentials).
   Keep any service account the scheduler uses.

**Rolling back from v1.0** to an older version is the usual image rollback (the schema rules in the deployment guide
still apply), **with the older deployment files**: move `repo-recovery-STAMP` back to `/opt/clawbridge/repo` before
recreating `api`. An older server with v1.0's `compose.yaml` and Google on refuses to start ("Google sign-in requires
--audience"), because that file no longer passes it. The older version still reads `CLAWBRIDGE_AUTH=device`, so it
keeps refusing Google until you change that setting and put the Google settings back.

## After the switch

- **A member with no device** (missed, lost laptop, all devices dormant): the main owner sends a recovery invite,
  `clawbridge invite --email EMAIL --recovery`, and the member joins with it ([sign-in](../sign-in.md#members-and-ownership)).
- **The main owner with no device:** the host operator issues a
  [bootstrap invite](../sign-in.md#the-operators-bootstrap-invite).
- **An old client or profile still on Google** gets the refusal above. `device enroll` needs a Google sign-in, which
  a device-only server no longer accepts, so the member asks an owner for an invite (or a recovery invite) and runs
  `clawbridge join`.
