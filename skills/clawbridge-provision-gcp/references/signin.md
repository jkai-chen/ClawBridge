# Sign-in: device keys

The server signs people in with device keys only: since v1.0 Google sign-in is
removed, and a server whose env file still names it refuses to start. The server
needs no Google OAuth client and no sign-in secret.

- **Env file (step 6):** leave `CLAWBRIDGE_AUTH` unset, or set it to `device`.
  Never set `google`, `CLAWBRIDGE_AUDIENCE` or any `CLAWBRIDGE_OAUTH_*` entry:
  `google` stops the server, and the old Google settings are ignored with a
  warning (`clawbridge cloud check server` warns while one is there). The
  server's public origin comes from `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and joins
  their first device:

  ```sh
  clawbridge cloud bootstrap VM --workspace my-team --email OWNER_EMAIL
  ```

  It runs the server's one-time owner invite over the user's own SSH and joins
  with it directly, so the invite code is never printed. When the onboarding
  TUI resumes after your hand-over, it runs this for the user. Never run the
  invite yourself, in any form: its code is a credential. If it fails, the user
  runs it again; a code that was issued expires in 24 hours.

If `clawbridge cloud bootstrap --help` doesn't list `--vm`, the user's
ClawBridge is too old: ask them to update it (`clawbridge self-update`). A host
operator can also let the owner in by hand with `operator bootstrap-invite`; see
docs/operations/deployment.md.
