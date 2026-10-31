# Sign-in: device keys

The server signs people in with device keys only: since v1.0 Google sign-in is
removed, and a server whose env file still names it refuses to start. The server
needs no Google OAuth client and no sign-in secret.

Check that the user's ClawBridge can bootstrap a Compute Engine VM, on their
machine:

```sh
clawbridge cloud bootstrap --help
```

- **It lists `--vm`:** use step 7 below.
- **It doesn't:** the user's ClawBridge is too old. Ask them to update it
  (`clawbridge self-update`), or use the manual step 7 below.

Then:

- **Env file (step 6):** `CLAWBRIDGE_AUTH=device`, set explicitly in the settings block: a release before 1.0
  defaults it to `google`, which needs Google settings this playbook doesn't create.
  Never set `google`, `CLAWBRIDGE_AUDIENCE` or any `CLAWBRIDGE_OAUTH_*` entry:
  `google` stops the server, and the old Google settings are ignored with a
  warning (`clawbridge cloud check server` warns while one is there). The
  server's public origin comes from `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and joins
  their first device:

  ```sh
  clawbridge cloud bootstrap --vm clawbridge-1 --project PROJECT --zone ZONE \
    --server https://DOMAIN --workspace my-team --email OWNER_EMAIL
  ```

  It makes the one-time owner invite's code on the user's machine, sends it
  only on the stdin of their own `gcloud compute ssh --tunnel-through-iap` to
  the server, and joins with it, so the code is never on screen or in a
  command line. It needs gcloud's own key on the user's machine: if it says to
  connect once, the user runs `gcloud compute ssh clawbridge-1 --project=PROJECT
  --zone=ZONE --tunnel-through-iap` in their own terminal, then runs it again.
  When your hand-over ends and its checks pass, `clawbridge onboard` offers to
  run this for the user.
- **Manual step 7** (a release without `--vm`): on the VM, over the user's own
  `gcloud compute ssh clawbridge-1 --project=PROJECT --zone=ZONE
  --tunnel-through-iap`, they run the `operator bootstrap-invite --workspace
  my-team` command from docs/operations/deployment.md ("Letting the owner in:
  `operator bootstrap-invite`"); then, on their own computer, `clawbridge
  --server https://DOMAIN join --email OWNER_EMAIL`, which asks for the code.

Never run the invite yourself, in any form, and never ask for the code: it is
a credential. If it fails, the user runs it again; a code that was issued
expires in 24 hours.
