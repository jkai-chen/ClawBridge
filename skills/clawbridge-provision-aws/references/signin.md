# Sign-in: device sign-in, or the Google fallback

Pick the path by the user's ClawBridge release. v0.3.4 brings device sign-in to
the server, and v0.3.5 adds `cloud bootstrap --instance`, the EC2 bootstrap over
SSM; with v0.3.4 or older use the Google fallback. The server must run the same
release or newer: its `operator bootstrap-invite` takes the code. Check the
release on the user's machine:

```sh
clawbridge cloud bootstrap --help
```

- **It succeeds and lists `--instance`:** the release has **device sign-in**
  and can bootstrap an EC2 instance. Use the device path. The server needs no
  Google OAuth client.
- **It succeeds without `--instance`:** the release has device sign-in, but not
  on an instance reached over SSM. SSM keeps every command's output in the
  account's history, and the bootstrap's one-time owner invite is a credential,
  so it never runs over plain `send-command`. Use the Google fallback, or ask
  the user to update ClawBridge first.
- **It fails with an unknown command:** the release predates device sign-in.
  Use the Google fallback.

## Device path (default)

- **Env file (step 6):** set `CLAWBRIDGE_AUTH=device`, and leave out
  `CLAWBRIDGE_AUDIENCE` and every `CLAWBRIDGE_OAUTH_*` entry. The server then
  signs people in with their devices only; its public origin comes from
  `CLAWBRIDGE_DOMAIN`.
- **Step 7:** the user, in their own terminal, creates the workspace and enrols
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
  anything is created. When the onboarding TUI resumes after your
  hand-over, it runs this for the user. **Never run the invite yourself, in any
  form:** its code is a credential. If it fails, the user runs it again; a code
  that was issued expires in 24 hours.

## Google fallback (releases without device sign-in over SSM)

Google sign-in needs OAuth clients in a **Google Cloud** project the user owns,
even though the server runs on AWS. If they have none, prefer updating
ClawBridge to a release with the device path.

The user creates these in that project's Google Cloud console (you can't
automate them; guide them click by click):

1. **OAuth consent screen:**
   - user type External, or Internal for a Google Workspace organisation;
   - app name "ClawBridge";
   - the owner's email as support and developer contact.

   While the screen is in *Testing*, add every person who will sign in as a test
   user.
2. **Two OAuth clients** (APIs & Services → Credentials → Create credentials →
   OAuth client ID): one **Desktop app**, one **TVs and Limited Input devices**.

**Env file (step 6):**
- Leave `CLAWBRIDGE_AUTH` unset; it defaults to Google.
- The client IDs are not secrets. Add them as `CLAWBRIDGE_OAUTH_CLIENT_ID` and
  `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID`, and both IDs, comma-separated, as
  `CLAWBRIDGE_AUDIENCE`.
- The two client secrets are secrets: stop and ask the user to run, in their own
  terminal:

```sh
clawbridge cloud secret set CLAWBRIDGE_OAUTH_CLIENT_SECRET --instance INSTANCE_ID --region REGION --profile PROFILE --account ACCOUNT_ID
clawbridge cloud secret set CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET --instance INSTANCE_ID --region REGION --profile PROFILE --account ACCOUNT_ID
```

Each value travels through a short-lived Parameter Store entry that the instance
reads with its own role. Then it's deleted, and it expires within 15 minutes in
any case. It never passes through you or through a command you send.

**Step 7:** the user signs in as the owner and creates the workspace:

```sh
clawbridge --server https://DOMAIN --audience AUDIENCE setup --cloud --name my-team
```
