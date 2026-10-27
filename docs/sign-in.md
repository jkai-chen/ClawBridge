# Signing in with invites and device keys

A hosted ClawBridge server can let people in without a Google account: an owner
invites them by email, they join with a one-time code, and from then on their
computer signs every request with its own **device key**.

> **Which release.**
> - **The server side shipped in v0.3.4:** `serve --auth`, `/v1/join`, signed requests,
>   invites, devices, members, ownership transfer, and `operator bootstrap-invite`.
> - **The client side ships in v0.3.5:**
>   - the client commands: `clawbridge join`, `invite`, `device`, `user`, `owner`,
>     `device enroll`, and the device checks in `whoami` and `doctor`;
>   - the TUI's Account screen and the guided setup's "I have an invite code" path;
>   - the `auth_mismatch` check in `clawbridge cloud check server`.
>
> A v0.3.4 client can't join a server with device keys: install v0.3.5 or later
> first ([install and update](installation.md)).

- [How it works](#how-it-works)
- [Invites and the one-time code](#invites-and-the-one-time-code)
- [Joining a device](#joining-a-device)
- [Device keys: the OS key store or a file](#device-keys-the-os-key-store-or-a-file)
- [Your devices](#your-devices)
- [Members and ownership](#members-and-ownership)
- [The operator's bootstrap invite](#the-operators-bootstrap-invite)
- [Turning device sign-in on](#turning-device-sign-in-on)
- [Troubleshooting](#troubleshooting)

## How it works

- **Roles.** The **main owner** is whoever created the workspace. Project
  **owners** and **leads** are as before.
- **Who may invite.** The main owner invites to any project and role. A project
  owner invites to their own projects, up to `lead`. A lead can't invite.
- **One code per invite.** An invite is for one email address. Its code works
  once, for that email only, and expires (72 hours by default, at most 7 days).
- **Your key.** Joining makes a key pair on your computer. The private half never
  leaves it; the server keeps the public half. Every request is signed with the key,
  so there is no password and no token to steal from the network.
- **One workspace per device.** A device belongs to the workspace whose invite
  admitted it, and signs only there. To join a second workspace, join again into
  another profile (`--client-config`); that makes a second key.
- **Google can still be used**, if the server offers it. Your Google sign-in and
  your devices are the same person to ClawBridge: the same email, roles and sessions.

## Invites and the one-time code

```sh
clawbridge invite --email ada@example.com --grant api=member --grant web=reader
clawbridge invite list
clawbridge invite revoke inv_7Q2M…
```

`invite` prints the code once, such as `CBJ-4K7P-WQ9D-2MXH-T6RB`, with the
invite's expiry. It prints it only to a terminal, never to an agent or a pipe:
`--out FILE` writes it to a new file instead (0600, never over an existing one),
which the invitee can pipe to `clawbridge join`. ClawBridge sends no email: **you** give the person the code and
the server's URL.

- **Send it like a password.** The code is bound to the email, but ClawBridge
  can't check that the person holds that mailbox. Whoever has the code and knows
  the email can join as that person until it expires. Use a channel you'd trust
  with a password, and revoke an invite that went astray.
- **Shown once.** The code is never stored or logged; `invite list` shows each
  invite's status (`pending`, `used`, `expired`, `revoked` or `burnt`) but never
  the code. A lost code means a new invite.
- **Five wrong tries burn it.** After five failed joins against one invite, it is
  `burnt`, and `invite list` says when. Anyone who knows the email can burn its
  invites by guessing, so a burnt invite is a nuisance, not a break-in: issue
  another.
- **Limits.** A workspace has at most 20 pending invites, and an email at most 3.
  The workspace's domain allow-list, if set, applies to the invited email.
- **An existing member** isn't invited again: they add a device themselves
  ([Your devices](#your-devices)), or the main owner issues a recovery invite.

In the TUI, the Account screen (`8`) issues an invite with `i`, typed as
`EMAIL PROJECT=ROLE ...`. The code shows in a box that holds the screen until
Enter or Esc, and is never drawn again.

## Joining a device

With the code and the server's URL:

```sh
clawbridge --server https://clawbridge.example.com join --email ada@example.com --label "Ada's laptop"
Invite code:            # typed, not shown
clawbridge --project api whoami      # identity_provenance: device-key
```

- **The code** is typed at the prompt, or piped on stdin. `--code` exists for
  scripts, but don't use it by hand: a command line is kept in shell history and
  shown in other users' process lists.
- **Where it goes.** `join` writes the profile (`~/.clawbridge/config.toml`, or
  the one `--client-config` names) to sign with the new key. The label is just a
  name, and defaults to the computer's name.
- **The guided way.** In a terminal, run bare `clawbridge`, choose **Join a team
  server**, give the URL, and choose **I have an invite code**. It asks for the
  email and a name for this device, shows your answers, and only after you confirm
  asks for the code, unechoed. It then picks your project (or asks, if you have
  several) and opens the TUI.
- **Already signed in with Google?** `clawbridge device enroll` adds a key to this
  computer with no invite. Your verified Google account is the proof. The profile
  then signs with the key, and the Google sign-in stays until `clawbridge logout`.

## Device keys: the OS key store or a file

By default the private key goes in the operating system's key store:

| System | Key store |
| --- | --- |
| macOS | the Keychain (it may ask you to allow access) |
| Windows | Credential Manager |
| Linux | the Secret Service (GNOME Keyring or KWallet), in your desktop or SSH session |

A machine with **no key store** (a headless Linux server, a container, WSL without
a keyring) can keep the key in a **file** instead:

```sh
clawbridge --server https://clawbridge.example.com join --email ada@example.com --key-store file
```

- **Where:** the key is a plain file under `~/.clawbridge/device/`, mode `0600`, in
  a `0700` directory. ClawBridge refuses to use it if its mode is wider or another
  user owns it.
- **What that means:** **anyone who can read the file as you can act as you.** Use
  it on machines only you can log in to, back nothing of it up, and revoke the
  device if the machine is shared or lost.
- **The guided setup** detects a missing key store, explains this, and asks before
  using a file.
- **Seeing it.** `whoami` and `doctor` show which store a profile uses. `doctor`
  checks that the key is readable and the file's mode is `0600`.

(Google sign-in, unlike device keys, always needs the OS key store; see
[Linux and WSL credentials](installation.md#linux-and-wsl-credentials).)

**CI** gets its own device rather than anyone's key: `clawbridge device add --ci
NAME --grant PROJECT=member --expires 30d` prints the key once, as
`CLAWBRIDGE_DEVICE_KEY`, for the CI secret store. It prints only to a terminal;
`--out FILE` writes `CLAWBRIDGE_DEVICE_KEY=…` to a new file (0600) instead. See
`clawbridge device --help`.

## Your devices

```sh
clawbridge device list                       # yours; this one marked
clawbridge device label dev_… "Ada's desktop"
clawbridge device revoke dev_…
clawbridge device add                        # a 24-hour code for another computer of yours
```

- **Another computer:** run `device add` on one you've already joined. It prints a
  code for your own email; on the new computer, `clawbridge join` with it.
- **Revoking** takes effect at once, on every server replica. Revoking the device
  you're on stops this profile working.
- **In the TUI:** the Account screen (`8`) lists your devices. `a` makes the code
  for another device after you type `add`, `l` labels, and `x` revokes after you
  type `revoke`.
- **Labels** are 1–64 printable characters. The server refuses control characters,
  terminal escapes and bidi or other format characters, because a label is shown on
  your other devices.

### New-device alerts

When a device is added to your account (by an invite, `device add`, recovery or
`device enroll`) or one of your devices is revoked, your **other** devices hear of
it, and so do the workspace owner's:

```text
New device "Ada's desktop" (dev_9k2…) joined ada@example.com at 2026-10-27 14:02 via device add. Not you? clawbridge device revoke dev_9k2… [dve_…]
```

- **Where:** after any command you run on one of those devices (on stderr), and as
  a banner on the TUI's Account screen. The line repeats until that device
  acknowledges it.
- **Acknowledging:** `clawbridge device ack --all`, or `device ack dve_…` for one;
  `e` on the Account screen. It's per device: acknowledging on your laptop leaves the
  alert on your desktop.
- **Listing:** `clawbridge device events` (`--all` includes acknowledged ones).
  Events older than 90 days aren't shown, and are deleted.
- **How many:** a command prints the newest five, then "…and N more"; so does the
  banner. Acknowledging clears them all.
- **Not alerted:** the device the event is about, the device that revoked a device
  itself, and CI devices. A device hears only of events since it was added. (The
  device that made an invite or `device add` code is still told when it's used.)
- **Not you?** Revoke the device from any other one of yours, then tell the
  workspace owner. A revoke is an alert too, so a stolen device can't quietly revoke
  your others.
- Alerts need a device-key sign-in; a Google sign-in has no device to alert.

## Members and ownership

The main owner sees and manages every member:

```sh
clawbridge user list                         # members, their roles and devices
clawbridge device list --member ada@example.com
clawbridge device revoke dev_…               # an owner may revoke anyone's
clawbridge user revoke ada@example.com --confirm
```

- **`user revoke`** removes the member from this workspace: every project role
  (retiring their sessions), every device of theirs here, the CI devices they
  created here, and their pending invites. The main owner can't revoke themselves;
  transfer ownership first.
- **A member who lost every device:** the main owner issues a recovery invite,
  `clawbridge invite --email ada@example.com --recovery`, and the member joins
  with it. Their other devices stay unless the owner adds `--revoke-others` (a
  "lost" laptop may just be at home). Since the owner holds that code, the owner
  can always act as any member of their workspace, and the audit records it.

**Transferring ownership** of the workspace:

```sh
clawbridge owner transfer --to bob@example.com     # the main owner offers
clawbridge owner show
clawbridge owner accept                            # Bob, from his own device
clawbridge owner decline | owner cancel
```

- **Who:** the offer goes to an existing member, and lasts 72 hours. There's at most
  one offer at a time.
- **On accept:** Bob becomes the main owner. The old owner keeps an explicit `owner`
  role on every project, which Bob may later remove. Pending recovery invites,
  including a bootstrap code, are revoked.
- **In the TUI:** the main owner offers with `t` and confirms by typing `transfer`.
  Bob sees a banner and answers with `y` or `n`, typing `accept` or `decline`; the
  owner can cancel with `c`.
- **Operators:** after a transfer, set `CLAWBRIDGE_BOOTSTRAP_OWNER` to the new
  owner, so a redeploy doesn't confuse readers. It never re-asserts the old owner
  on an existing workspace.

## The operator's bootstrap invite

On a server that takes device keys only, nobody can sign in until the owner has a
device. The host operator lets the owner in with a one-time code, run on the
server host in your own terminal:

```sh
clawbridge operator bootstrap-invite --workspace WORKSPACE
```

- It creates the workspace for `CLAWBRIDGE_BOOTSTRAP_OWNER` if needed, and prints
  a code for that email, valid for 24 hours. The owner then runs `clawbridge join`
  with it on their own computer.
- The same command recovers an owner who has lost every device.
- The code is a secret. The command refuses to print it anywhere but a terminal
  unless given `--force-stdout`.

The Compose form and the details are in
[Deployment: letting the owner in without Google](operations/deployment.md#letting-the-owner-in-without-google-operator-bootstrap-invite).

## Turning device sign-in on

The server's sign-in methods are `CLAWBRIDGE_AUTH` in its environment file
(`serve --auth`):

| `CLAWBRIDGE_AUTH` | Accepts |
| --- | --- |
| unset or `google` | Google sign-in only (the default, unchanged on upgrade) |
| `google,device` | Google and device keys |
| `device` | device keys only |

Device keys need `CLAWBRIDGE_DOMAIN`, the server's public origin. The
[deployment guide](operations/deployment.md) has every setting.

**Upgrade the clients first, then switch.**

1. **Upgrade the server,** and keep `google`, or go to `google,device`. Nobody is
   locked out: Google keeps working, and clients that know device keys can join or
   enrol.
2. **Upgrade every member's client** to the release with `join` and
   `device enroll`.
3. **Give every member a device.** Members signed in with Google run
   `clawbridge device enroll`; new people join with an invite. Check with
   `clawbridge user list` that each member has an active device, and that you do.
4. **Only then set `device`.** From then on the server refuses Google tokens
   everywhere. An old client, or a profile still on Google, gets "a
   ClawBridge-Device signature is required; this server accepts device keys only",
   and `device enroll` is refused too.

**Set `device` too early?** If members are locked out, set `CLAWBRIDGE_AUTH` back
to `google,device` and recreate the `api` container. Nothing stored is touched, and
Google sign-in works again at once. An owner with no device can also be let in
with the [bootstrap invite](#the-operators-bootstrap-invite).

The other way round, a new client against a server older than device sign-in says
"this server doesn't support invites; sign in with clawbridge login".

After changing `CLAWBRIDGE_AUTH`, recreate the `api` container so it reads the
file, then check:

```sh
clawbridge cloud check server --ssh HOST      # fails auth_mismatch until the running server matches
curl -s https://clawbridge.example.com/v1/config    # auth_methods, public_origin
```

## Troubleshooting

- **"cannot save the device key in the OS key store"** (or read). The key store is missing,
  locked, or not in this session.
  - On Linux, run ClawBridge in the desktop or SSH session where your keyring is
    unlocked (see [Linux and WSL credentials](installation.md#linux-and-wsl-credentials)).
  - On a machine with no key store, join with `--key-store file`, after reading
    [what that means](#device-keys-the-os-key-store-or-a-file).
  - A failed join leaves no key and no profile, so you can simply try again.
- **"invalid or expired invite".** The code, email, expiry or status doesn't match,
  and the server doesn't say which. Check the email is the invited one and the code
  is complete (case, spaces and dashes don't matter). Ask the inviter to check
  `invite list`; a `used`, `burnt` or `expired` invite needs a new one.
- **"the server's answer to the join was lost".** The device may or may not have
  been added. Its key was kept and the error says where. If the invite now shows
  `used`, ask an owner to revoke that device and issue another invite.
- **A lost or stolen device.** Revoke it from another of your devices
  (`device revoke`, or `x` on the Account screen), or ask an owner to. With no
  device left, ask the main owner for a recovery invite. A main owner who has lost
  every device asks the host operator for a
  [bootstrap invite](#the-operators-bootstrap-invite).
- **"this device was revoked or has expired; ask an owner for an invite".** This
  device can no longer sign. Join again with a new invite; for another of your
  devices, `device add` makes the code.
- **"the request time is more than 300 s from the server's".** The clock is off.
  The client corrects once by the server's time, and the correction lapses after 24
  hours; `doctor` shows it. Over an hour off is refused: fix the system clock
  (enable network time).
- **`auth_mismatch` from `clawbridge cloud check server`.** The environment file
  says one set of sign-in methods, but the running server uses another. The server
  reads the file only when the `api` container is created, so recreate it
  (`docker compose … up -d --no-deps api`, see the
  [deployment guide](operations/deployment.md)) and check again.
- **"this profile already signs as device …".** A profile holds one device. To join
  another workspace, or the same one again from scratch, use another profile:
  `--client-config /absolute/path/other.toml`.
