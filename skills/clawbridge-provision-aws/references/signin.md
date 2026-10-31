# Sign-in: device keys

The server signs people in with device keys only: since v1.0 Google sign-in is
removed, and a server whose env file still names it refuses to start. The server
needs no Google OAuth client and no sign-in secret.

Check that the user's ClawBridge can bootstrap an EC2 instance over SSM, on
their machine:

```sh
clawbridge cloud bootstrap --help
```

If it doesn't list `--instance`, the release is too old: ask the user to update
ClawBridge first (`clawbridge self-update`). SSM keeps every command's output in
the account's history, and the bootstrap's one-time owner invite is a
credential, so it never runs over plain `send-command`. The server must run the
same release or newer: its `operator bootstrap-invite` takes the code.
The code must be one `clawbridge` generated (the server refuses a weaker one), so
never make one by hand.

- **Env file (step 6):** `CLAWBRIDGE_AUTH=device`, set explicitly in the settings block: a release before 1.0
  defaults it to `google`, which needs Google settings this playbook doesn't create.
  Never set `google`, `CLAWBRIDGE_AUDIENCE` or any `CLAWBRIDGE_OAUTH_*` entry:
  `google` stops the server, and the old Google settings are ignored with a
  warning (`clawbridge cloud check server` warns while one is there). The
  server's public origin comes from `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and joins
  their first device:

  ```sh
  clawbridge cloud bootstrap --instance INSTANCE_ID --region REGION --profile PROFILE --account ACCOUNT_ID \
    --server https://DOMAIN --workspace my-team --email OWNER_EMAIL
  ```

  It makes the one-time owner invite on the user's machine, hands it to the
  server through the same short-lived encrypted parameter as `cloud secret
  set`, and joins with it, so the code never appears in SSM command history or
  on screen. The server keeps only a salted hash of it. `--email` is checked
  against the env file's `CLAWBRIDGE_BOOTSTRAP_OWNER`; a mismatch stops before
  anything is created. When your hand-over ends and its checks pass,
  `clawbridge onboard` offers to run this for the user. **Never run the invite yourself, in any
  form:** its code is a credential. If it fails, the user runs it again; a code
  that was issued expires in 24 hours.
