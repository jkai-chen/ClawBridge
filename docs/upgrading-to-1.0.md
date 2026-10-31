# Upgrading to ClawBridge 1.0

This page is for anyone who runs their own ClawBridge server, on Google Cloud, AWS, a VM you reach over SSH, or your
own machine, and for the members who use it. It covers:
- what 1.0 changes;
- the order to upgrade in;
- how to check that every member is ready;
- how to roll back.

## What changes in 1.0

- **Device keys are the only sign-in.** Google sign-in (the browser OAuth flow, `clawbridge login`, a Google ID
  token on requests, and gcloud/ADC as a way to sign in) is removed.
  - A 1.0 server **refuses to start** while `CLAWBRIDGE_AUTH` names `google`.
  - A client signed in with Google is refused, with how to join instead.
  - Unchanged: Google Cloud as a place to host your server (`clawbridge cloud …` with your own `gcloud`), and a Cloud
    Scheduler tick's OIDC token.
- **The deployment files changed.** `deploy/compose.yaml` defaults `CLAWBRIDGE_AUTH` to `device` and no longer passes
  `--audience` or the OAuth client settings.
- **The database:** 1.0 runs schema 54, the same as v0.3.13. Coming from an older release, the usual one-way schema
  rules in the [deployment guide](operations/deployment.md#update-and-rollback) apply.
- **The release process** behind 1.0 (a release held as a verified draft, then published unchanged) is internal to
  ClawBridge's maintainers. You don't need to do anything for it.

**Already on device keys?** A server set up with `deploy/local-host.sh`, or one that already runs
`CLAWBRIDGE_AUTH=device`, upgrades as usual: skip to [Upgrade the server](#3-upgrade-the-server).

## The order

**1.0 is published a few days after it is cut.** Until the release is published, the installer and
`clawbridge self-update` still give the release before it, and the 1.0 image and its `IMAGE-DIGEST.txt` are not
available yet. Steps 1 and 2 need nothing from 1.0: do them now, and steps 3 to 5 once 1.0 is published
([releases](https://github.com/jkai-chen/ClawBridge/releases)).

Members move first, then the server's setting, then the server's version. A 1.0 server never accepts Google, so
nobody can be moved to a device key after the upgrade by signing in with Google.

### 1. Every member has a device key, on your current version

Do this while the server still accepts both methods (`CLAWBRIDGE_AUTH=google,device`). Each member who signs in with
Google runs `clawbridge device enroll` in their own terminal, **with the client they have now**: 1.0's client no
longer has `device enroll`, so members update their clients only after this step (step 4). New people join with an invite (`clawbridge join`).
[Removing Google sign-in](operations/google-sign-in-removal.md#before-you-switch) has the steps, including CI devices
and agent sessions.

**Check, as an owner, from a device-signed profile:**

```sh
clawbridge user list                          # DEVICES: "N active of M" for each member
clawbridge device list --member EMAIL         # STATUS (active, dormant …) and LAST USED, per device
clawbridge --project PROJECT whoami           # "identity_provenance":"device-key"
```

A member is ready when they have at least one **active** device that isn't dormant, and its LAST USED moves when
they work. A member who has used ClawBridge recently but whose devices show LAST USED "never", or nothing recent, is
still signing in with Google: they aren't ready.

**Check from the database (optional, for operators).** This is read-only, and it is what ClawBridge's own hosted
server runs before its switch. A Google sign-in isn't recorded as such, so the check infers it: a member whose
sessions were active in the last 7 days, with no device used in that time, is still on Google. Run it with `psql`
against the server's database:

```sql
BEGIN READ ONLY;
WITH t AS (SELECT to_char(now() AT TIME ZONE 'UTC' - interval '7 days','YYYY-MM-DD"T"HH24:MI:SS.US"000Z"') AS since,
                  to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"000Z"') AS now_s),
dev AS (SELECT d.workspace_id, d.principal,
               count(*) FILTER (WHERE d.revoked_at IS NULL AND (d.expires_at IS NULL OR d.expires_at > t.now_s)) AS live_devices,
               max(d.last_used_at) FILTER (WHERE d.revoked_at IS NULL) AS last_device_use
        FROM devices d, t GROUP BY d.workspace_id, d.principal),
act AS (SELECT p.workspace_id, s.owner_principal AS principal, max(s.last_seen) AS last_session_seen
        FROM sessions s JOIN projects p ON p.id=s.project_id GROUP BY p.workspace_id, s.owner_principal)
SELECT * FROM (
SELECT m.principal, coalesce(dev.live_devices,0) AS live_devices, dev.last_device_use, act.last_session_seen,
       CASE
         WHEN act.last_session_seen >= t.since AND coalesce(dev.live_devices,0)=0 THEN 'NOT READY: active, no live device'
         WHEN act.last_session_seen >= t.since AND (dev.last_device_use IS NULL OR dev.last_device_use < t.since) THEN 'NOT READY: active, no device use in 7 days'
         WHEN coalesce(dev.live_devices,0)=0 THEN 'inactive, no device (recovery invite later)'
         ELSE 'ready'
       END AS verdict
FROM members m CROSS JOIN t
LEFT JOIN dev ON dev.workspace_id=m.workspace_id AND dev.principal=m.principal
LEFT JOIN act ON act.workspace_id=m.workspace_id AND act.principal=m.principal
) g ORDER BY verdict LIKE 'NOT READY%' DESC, principal;
ROLLBACK;
```

Every `NOT READY` member moves to a device key first, or is left out on purpose: they join later with a recovery
invite.

### 2. Switch the server to device keys, still on your current version

1. Set `CLAWBRIDGE_AUTH=device` in the server's environment file, and recreate the `api` container.
2. Check that `/v1/config` shows `"auth_methods":["device"]` and that your `whoami` still says `device-key`.
3. Leave it for a day if you can. Anyone still on Google now gets "a ClawBridge-Device signature is required". In
   `clawbridge device list`, every member who works keeps a moving LAST USED.

**Keep the Google settings for now** (`CLAWBRIDGE_AUDIENCE` and the four `CLAWBRIDGE_OAUTH_*`). Under `device` they
are unused, and they let you switch back to `google,device` at once if someone was missed.

### 3. Upgrade the server

- **The deployment files:** put the 1.0 release's source in place before recreating `api`, as the
  [deployment guide](operations/deployment.md#update-and-rollback) describes, and keep the old directory for a
  rollback. Older files still start 1.0 (an empty `--audience` is ignored), but they default `CLAWBRIDGE_AUTH` to
  `google`, so step 2 must be done.
- **The image:** with the [published image](operations/deployment.md#the-published-image-v036-and-later), set
  `CLAWBRIDGE_IMAGE` to the digest the 1.0 release names (its `IMAGE-DIGEST.txt`). You can verify it first:

  ```sh
  gh attestation verify oci://ghcr.io/jkai-chen/clawbridge@sha256:DIGEST --repo jkai-chen/ClawBridge \
    --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref refs/tags/v1.0.0
  ```

- **On your own machine:** follow the [local host upgrades](operations/local-host.md#upgrades).
- **Then:** drain, swap and check as usual. If the new container exits at once, read its one log line. Usually
  `CLAWBRIDGE_AUTH` still names `google`: set `device` and start it again.
- **After:** `/healthz` shows the 1.0 version, `/v1/config` shows `"auth_methods":["device"]`, and a device-signed
  `whoami` works.

### 4. Update the clients

Each member runs `clawbridge self-update` ([install and update](installation.md#upgrade)). An agent's MCP entry that
was installed while its person signed in with Google stops with a message to re-run `clawbridge mcp install` after
joining. Entries installed on a device key keep working.

### 5. Remove the Google settings, when you're sure

Once you won't roll back to Google:
- delete `CLAWBRIDGE_AUDIENCE`, `CLAWBRIDGE_OAUTH_CLIENT_ID`, `CLAWBRIDGE_OAUTH_CLIENT_SECRET`,
  `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID` and `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET` from the environment file (and your
  secret store);
- keep `CLAWBRIDGE_TICK_*`.

`clawbridge cloud check server` warns until they're gone. 1.0 ignores them. You can then delete the OAuth client in
the Google Cloud console.

## Rolling back

- **After step 2, still on the old version:** set `CLAWBRIDGE_AUTH=google,device` again and recreate `api`. Google
  sign-in works again at once; nothing stored changed.
- **After step 3:** the usual image rollback, with the **older deployment files** put back. With
  `CLAWBRIDGE_AUTH=device` still set, the older version starts in device mode and members are unaffected. Going back
  to Google from there also needs the Google settings, which is why step 5 waits.
- [Removing Google sign-in](operations/google-sign-in-removal.md#deploy-v10) has the details.

## A member who was missed

They ask an owner for a **recovery invite** (`clawbridge invite --email EMAIL --recovery`) and run `clawbridge
join`. An owner with no device asks the host operator for a
[bootstrap invite](sign-in.md#the-operators-bootstrap-invite).
