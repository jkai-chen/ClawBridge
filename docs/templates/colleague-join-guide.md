# Joining PROJECT on ClawBridge: template

Copy this page for a new colleague, replace the made-up values (`PROJECT`,
`https://clawbridge.example.test`, `OWNER`, `LANE`, the team names), and keep the one
variant that matches their machine. It isn't seeded into Linear; send it with their
invite. The owner's side is in [onboarding](../onboarding.md#for-the-project-owner);
what to do after they join is under [After they join](#after-they-join-owner-and-lead-checklist).

Needs ClawBridge v0.3.12 or later on the colleague's machine (v0.3.13 for the WSL
Linear file store). About 20 minutes.

---

## Before you start (every variant)

- **Your Linear invitation accepted,** and you're in the teams OWNER named
  (for example `Platform`, `Review`).
- **The invite link and the server address** OWNER sent you
  (`https://clawbridge.example.test`). The link works once, only for your email, and
  expires 72 hours after it was made (at most 7 days). Treat it like a password: paste
  it only into ClawBridge. Five failed joins with it (expired, a key already in use…) burn it.
- **Claude Code and/or Codex** installed where you'll run them (on WSL, inside Ubuntu).

## Variant A: macOS or Linux

1. **Install.**

   ```sh
   curl -fsSL --proto '=https' --proto-redir '=https' https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
   export PATH="$HOME/.local/bin:$PATH"
   clawbridge --version
   ```

   Add the `export PATH=...` line to your shell's startup file (`~/.zshrc` or
   `~/.bashrc`). The installer also installs the ClawBridge skill for Claude Code and
   Codex when they're set up.
2. **Join.** In the repository you open your agent in, run `clawbridge` and follow
   [Join and the guided setup](#join-and-the-guided-setup). The device key and your
   Linear sign-in go to the OS key store (Keychain on macOS, Secret Service on Linux).

## Variant B: native Windows (PowerShell)

1. **Install.** In PowerShell:

   ```powershell
   Invoke-WebRequest -Uri 'https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.ps1' -OutFile "$env:TEMP\clawbridge-install.ps1"
   powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\clawbridge-install.ps1"
   clawbridge --version
   ```

   It picks the ARM64 build on an ARM machine, even from an emulated x64 PowerShell.
2. **Join.** In the repository folder, run the bare `clawbridge` and follow
   [Join and the guided setup](#join-and-the-guided-setup). The device key and your
   Linear sign-in go to Windows Credential Manager.
3. **Hooks.** `mcp install` (step 3 of 5 below) writes Claude Code's hooks in exec form
   (`clawbridge.exe` with arguments, no shell); `clawbridge doctor` reports the hook
   form and Claude Code's version. Waking an idle agent through them on Windows is
   still being confirmed, so tell your agent to check its inbox at the start and end of
   every turn, and nudge it ("check your ClawBridge inbox") when you expect a message.

## Variant C: WSL (Ubuntu on Windows)

Your agent runs inside WSL, so ClawBridge goes there too: the Linux build, installed in
your Ubuntu shell, not the PowerShell one. Everything below happens in that Ubuntu shell.

1. **Keep your repositories in Ubuntu's home** (`~/...` in the Ubuntu shell), not under `/mnt/c`:
   ClawBridge keeps private files there, and `/mnt/c` can't keep them private: its
   private-file checks fail on Windows drives, and from v0.3.13 the Linear file store
   refuses a state directory under `/mnt/`.
2. **Install** with Variant A's commands, inside Ubuntu, and add the `PATH` line to
   `~/.bashrc`.
3. **Choose where the secrets live.** WSL has no OS key store by default.
   - **v0.3.13 and later (recommended):** use files. At "Keep the device key in a file? [y/N]"
     answer `y`; the guided setup asks the same for your Linear
     sign-in when it finds no Secret Service. Outside the setup, pass the flag yourself:
     `clawbridge join --key-store file` and
     `clawbridge linear connect --name Work --access write --key-store file`. Both files are 0600
     in a 0700 folder in your Ubuntu home; anyone who can act as your Linux user can
     use them. `linear status` shows `key_store`, and doctor's `linear.connection`
     check names the store.
   - **v0.3.12 and older:** the device key can use a file (`--key-store file`), but the
     Linear sign-in needs a Secret Service. Set one up once:
     `sudo apt-get install -y gnome-keyring libsecret-tools dbus-user-session`, make
     sure `/etc/wsl.conf` has `[boot]` and `systemd=true`, run `wsl --shutdown` in
     PowerShell and reopen Ubuntu. Then **unlock it once per WSL session**, before
     ClawBridge or your agent: `gnome-keyring-daemon --replace --unlock --components=secrets`
     (type the keyring password, then Ctrl-D). Two traps:
     - ClawBridge saves into the keyring collection named `login` when it exists (else
       the `default` one). The trap: a locked `login` collection while `secret-tool`,
       which uses the default, still works. Check
       `busctl --user get-property org.freedesktop.secrets /org/freedesktop/secrets/collection/login org.freedesktop.Secret.Collection Locked`
       (expect `b false`, or no such collection).
     - **Only one `gnome-keyring-daemon` may run.** A second one answers on the bus with a
       locked or empty keyring, and sign-in fails with "secure credential storage
       failed". `pgrep -a gnome-keyring` should list one; if it lists two, stop both and
       unlock again.
4. **Join.** Run `clawbridge` in the repository and follow
   [Join and the guided setup](#join-and-the-guided-setup). When it prints a Linear
   sign-in link, open it in your Windows browser.
5. **Each new WSL session** (after a restart of WSL): on v0.3.12 and older, unlock the
   keyring first. Then start your agent from the same Ubuntu shell. New messages wake
   it as they do on Linux.

## Join and the guided setup

Run `clawbridge` in the repository you open your agent in (its settings are written
there), and answer:

1. **How you want to start:** Join a team server.
2. **Service HTTPS URL:** the server address OWNER sent you.
3. **How do you sign in?** I have an invite code (some servers go straight to it).
4. **Email:** leave it empty when you have an invite link.
5. **Name for this device:** for example `ada laptop`.
6. **Check the service and device** it shows, and answer `y`.
7. **Invite link or code:** paste the link. It isn't shown as you type.

You'll see "Joined workspace …". There's no ClawBridge password. Choose the project
`PROJECT` if you're asked. Then the guided setup takes five short steps, each written
only after your `y`:

| Step | Answer |
| --- | --- |
| 1 of 5 · Agent skill | `y` (installs the skill for Claude Code and Codex) |
| 2 of 5 · Linear | `y`, name it `Work`, and type `write` at "Access read/write". Open the link it prints and approve in Linear. |
| 3 of 5 · MCP | `y` for each agent, each with its own session name (`ada-claude`, `ada-codex`). This also installs the Claude Code hooks. |
| 4 of 5 · Doctor | Runs the checks; it should show no `fail`. |
| 5 of 5 · Dispatcher | Nothing to answer for a member: "skipped: the project owner sets the dispatcher". |

Skipped a step? The summary at the end prints its command.

## Start your agent

Restart Claude Code or Codex so it loads ClawBridge, then send it this first message:

> You're connected to ClawBridge (project `PROJECT`). Read the ClawBridge skill first
> and follow it. Register your session, then send the project's coordinator (named in
> `whoami`) a one-line hello with your session name. Check your inbox, and confirm your
> lane is `LANE`. Take issues only from me or the coordinator; propose an assignment
> and wait for the coordinator's confirmation before editing code. Never mention
> anyone. Check your inbox at the start and end of every turn.

The coordinator is told when your session registers, so it can answer your hello.

## Agent sessions: one MCP entry each

- **One `mcp install` per agent session,** in the repository it works in, never a
  global entry shared by several agents:
  `clawbridge mcp install --agent claude-code --session ada-claude` (Claude Code writes
  `.mcp.json` and its hooks in that folder),
  `clawbridge mcp install --agent codex --session ada-codex` (add `--dry-run` first to
  see what it writes). Add `--linear-profile PROFILE_ID` to pin a Linear connection;
  doctor accepts that entry from v0.3.13.
- **Two sessions in one folder.** Each entry is bound to one session, so two agents that
  share a project folder can't both use its `.mcp.json`. Give each its own file and
  launch each Claude Code with it:

  ```sh
  clawbridge mcp install --agent claude-code --session ada-claude-1 --config /absolute/path/ada-claude-1.mcp.json
  claude --mcp-config /absolute/path/ada-claude-1.mcp.json --strict-mcp-config
  clawbridge doctor --mcp-agent claude-code --mcp-config /absolute/path/ada-claude-1.mcp.json --mcp-session ada-claude-1
  ```

  (`--strict-mcp-config` keeps Claude Code from also loading the folder's `.mcp.json`.)
  Both share the folder's hooks, which bind to whichever `CLAWBRIDGE_SESSION` is in that
  Claude Code's environment: export it before each launch (see the next point). If both
  launch with the same `CLAWBRIDGE_SESSION` (or none), the second can't take the first's
  binding while it runs, so its context watch and its Stop hook (the check for waiting
  messages before it idles) don't work for it.
  Distinct `--name` values put several entries in one file instead, but then each
  Claude Code loads all of them, which you usually don't want.
- **Relaunching an agent by hand** outside its MCP entry (a script, a second terminal):
  export the session's scope first, so it keeps the same identity:

  ```sh
  export CLAWBRIDGE_WORKSPACE=WORKSPACE CLAWBRIDGE_PROJECT=PROJECT
  export CLAWBRIDGE_SESSION=ada-claude CLAWBRIDGE_INSTANCE=INSTANCE   # from register / whoami
  ```

  Claude Code's hooks bind to the session only when `CLAWBRIDGE_SESSION` is in Claude
  Code's own environment, and they need `clawbridge` on its `PATH`.

- **Shell commands don't inherit the MCP server's scope.** A `clawbridge` command an
  agent runs in its shell passes the server's scope as flags:
  `clawbridge --workspace WORKSPACE --project PROJECT --session ada-claude --instance INSTANCE inbox`
  (or uses the exported variables above). Otherwise it falls back to the checkout's
  files, which may name another project. `clawbridge linear context --json` shows the
  scope it used and where each value came from.
- **Waking an idle agent.** Claude Code's Stop hook covers a busy agent. For an idle
  one, run a notifier per session that types a fixed line into its terminal:

  ```sh
  # Orca
  clawbridge --session ada-claude --instance INSTANCE notify --adapter command \
    --state /absolute/private/ada-claude-notify.json -- \
    orca terminal send --terminal TERMINAL_HANDLE --text {text} --enter
  # tmux
  clawbridge --session ada-claude --instance INSTANCE notify --adapter command \
    --state /absolute/private/ada-claude-notify.json -- \
    tmux send-keys -t SESSION:WINDOW.PANE {text} Enter
  ```

  It only ever sends `N ClawBridge messages are waiting; call inbox`, never a sender or
  body ([notifications](../notifications.md)).

## If something goes wrong

| What you see | What to do |
| --- | --- |
| `clawbridge` isn't found | Add `$HOME/.local/bin` to `PATH` (Windows: open a new PowerShell). |
| The invite is refused, expired or burnt | Ask OWNER for a new invite. |
| `join` or `linear connect` hangs with no output | Your OS key store is waiting for an unlock prompt you can't see. Stop it and unlock the key store, or use `--key-store file` (WSL, headless Linux). |
| WSL: "secure credential storage failed" | See Variant C step 3: one daemon, `login` unlocked; or use the file store on v0.3.13. |
| Linear worked yesterday but fails today (WSL, keyring) | WSL restarted: unlock the keyring again. |
| Your agent gets `outside_lane` | Your session isn't in a lane that writes that team. Tell OWNER. |
| Your agent can't see an issue | Check you're in that Linear team. |
| Anything else | Run `clawbridge doctor` and send OWNER the lines marked `fail`. |

---

## After they join (owner and lead checklist)

From your own terminal, without `--session`. Each command previews until you add
`--confirm`; read the revision `N` from the matching `show`.

- [ ] **Team role, linked to their account:**
  `clawbridge linear roles person "Ada Example" --role engineer --team TEAM_UUID --link ada@example.test --expected-revision N`.
  Without `--link` the role grants nothing. Only the owner links a lead's row; a team's
  lead sets `engineer` and `reviewer` for the teams they lead.
- [ ] **Lane:** `clawbridge linear lanes pin --pin-principal ada@example.test --lane LANE --expected-revision N`
  (or `--pin-session NAME` for one session). With `unpinned: read_only`, an unpinned
  newcomer reads but can't write; the first refusal reads `outside_lane`.
- [ ] **Dispatcher (rules), if you use one:** trust their session in its domain,
  `clawbridge dispatcher domain trust DOMAIN SESSION --expected-revision N`. From v0.3.13,
  `dispatcher enable … --confirm` really enables (before, it only previewed).
- [ ] **Who closes:** `clawbridge linear closes show`; change it with
  `closes set --linear-project UUID --human|--agent --expected-revision N` (never from a session).
- [ ] **Who may cancel (v0.3.13), owner or lead:** `clawbridge linear cancel-kinds show`. Agents
  never cancel or mark Duplicate unless a person lists the project's kind:
  `clawbridge linear cancel-kinds set --kinds Engineering --expected-revision N` (you type the
  kind names; `C` on the TUI's Linear screen does the same). It needs the project kinds set,
  seeded and applied as Linear project labels (`linear kind set`, `kind seed`, `kind apply`,
  the owner's), and a hosted server with the ClawBridge Linear app, which reads the kind at
  write time; without them every agent cancel is refused with `kind_unknown`.
- [ ] **Check:** `clawbridge doctor` as yourself, and the coordinator's session-joined notice
  when their agent registers.
