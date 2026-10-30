# Sign-in: device sign-in

This route needs a ClawBridge release with `cloud bootstrap --ssh`. Check it on
the user's machine:

```sh
clawbridge cloud bootstrap --help
```

- **It lists `--ssh`:** use the device path below. The server needs no Google
  OAuth client.
- **It doesn't:** the release predates this route. Ask the user to update
  ClawBridge first, then start again. The server must run the same release or
  newer, since its `operator bootstrap-invite` takes the code.

## Device path

- **Env file (step 4):** `CLAWBRIDGE_AUTH=device` is in the settings block.
  Leave out `CLAWBRIDGE_AUDIENCE` and every `CLAWBRIDGE_OAUTH_*` entry. The
  server then signs people in with their devices only, and its public origin
  comes from `CLAWBRIDGE_DOMAIN`.
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

Google sign-in on this route follows "Sign-in methods" in
`/opt/clawbridge/repo/docs/operations/deployment.md`; its two client secrets
are the user's to enter with `clawbridge cloud secret set KEY --ssh HOST`.
