# Sign-in: device keys

The server signs people in with device keys only: since v1.0 Google sign-in is
removed, and a server whose env file still names it refuses to start.

This route needs a ClawBridge release with `cloud bootstrap --ssh`. Check it on
the user's machine:

```sh
clawbridge cloud bootstrap --help
```

- **It lists `--ssh`:** use the device path below.
- **It doesn't:** the release predates this route. Ask the user to update
  ClawBridge first, then start again. The server must run the same release or
  newer, since its `operator bootstrap-invite` takes the code.

## Device path

- **Env file (step 4):** `CLAWBRIDGE_AUTH=device` is in the settings block.
  Never set `google`, `CLAWBRIDGE_AUDIENCE` or any `CLAWBRIDGE_OAUTH_*` entry:
  `google` stops the server, and the old Google settings are ignored with a
  warning (`clawbridge cloud check server` warns while one is there). The
  server's public origin comes from `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and enrols
  their first device:

  ```sh
  clawbridge cloud bootstrap --ssh HOST --server https://DOMAIN --workspace my-team --email OWNER_EMAIL
  ```

  It makes the one-time owner invite on the user's machine and sends it only on
  ssh's stdin to the server, then joins with it, so the code never appears on
  screen or in a command line. The server keeps only a salted hash of it.
  `--email` is checked against the env file's `CLAWBRIDGE_BOOTSTRAP_OWNER`; a
  mismatch stops before anything is created. When your hand-over ends and its
  checks pass, `clawbridge onboard` offers to run this for the user. **Never
  run the invite yourself, in any form:** its code is a credential. If it
  fails, the user runs it again; a code that was issued expires in 24 hours.
