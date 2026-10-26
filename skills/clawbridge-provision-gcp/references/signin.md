# Sign-in: device sign-in, or the Google fallback

Pick the path by the user's ClawBridge release, on their machine:

```sh
clawbridge cloud bootstrap --help
```

- It succeeds: the release has **device sign-in**, and its compose file no
  longer requires the Google entries. Use the device path. The server needs no
  Google OAuth client.
- It fails with an unknown command: the release predates device sign-in. Use
  the Google fallback.

## Device path (default)

- **Env file (step 6):** set `CLAWBRIDGE_AUTH=device`, and leave out
  `CLAWBRIDGE_AUDIENCE` and every `CLAWBRIDGE_OAUTH_*` entry. The server then
  signs people in with their devices only; its public origin comes from
  `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and enrols
  their first device:

  ```sh
  clawbridge cloud bootstrap VM --workspace my-team --email OWNER_EMAIL
  ```

  It runs the server's one-time owner invite over the user's own SSH and joins
  with it directly, so the invite code is never printed. When the onboarding
  TUI resumes after your hand-over, it runs this for the user. Never run the
  invite yourself, in any form: its code is a credential. If it fails, the user
  runs it again; a code that was issued expires in 24 hours.

## Google fallback (releases before device sign-in)

The user creates, in the Google Cloud console of the same project (you cannot
automate these; guide them click by click):

1. **OAuth consent screen:** user type External (or Internal for a Google
   Workspace organisation), app name "ClawBridge", the owner's email as support
   and developer contact. While in *Testing*, add every person who will sign in
   as a test user.
2. **Two OAuth clients** (APIs & Services → Credentials → Create credentials →
   OAuth client ID): one **Desktop app**, one **TVs and Limited Input devices**.

Leave `CLAWBRIDGE_AUTH` unset (it defaults to Google). The client IDs are not
secrets: put them in the env file in step 6 as
`CLAWBRIDGE_OAUTH_CLIENT_ID` and `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID`, and both IDs,
comma-separated, as `CLAWBRIDGE_AUDIENCE`. The two client secrets are: stop and
ask the user to run, in their own terminal,

```sh
clawbridge cloud secret set CLAWBRIDGE_OAUTH_CLIENT_SECRET VM
clawbridge cloud secret set CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET VM
```

**Step 7:** the user signs in as the owner and creates the workspace:

```sh
clawbridge --server https://DOMAIN --audience AUDIENCE setup --cloud --name my-team
```
