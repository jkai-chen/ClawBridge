# Install and update

Every published release on [GitHub Releases](https://github.com/jkai-chen/ClawBridge/releases)
carries versioned binaries, installers and SHA256 checksums. Source builds remain
available below. In the pinned examples, replace `vX.Y.Z` with a release tag.

For macOS or Linux, the installer selects Intel/AMD64 or ARM64, verifies the
release checksum and installs into `~/.local/bin`:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

Already installed? Upgrade with `clawbridge self-update` (see [Upgrade](#upgrade)).

To pin the installer and binary version:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/download/vX.Y.Z/install.sh \
  | sh -s -- --version vX.Y.Z
```

Use `--bin-dir /absolute/path` to choose another destination. After installing,
the installer runs the new binary once as `clawbridge skill install`, which
installs the [agent skill](#agent-skill) for the agents configured on this machine;
`--no-skill` skips that. A skill failure is a warning; the binary stays installed.
On macOS and Linux the installer prints the PATH step; it does not change shell
profiles or PATH itself. The `export` above lasts for the current shell only; to
keep it, add `export PATH="$HOME/.local/bin:$PATH"` to `~/.zshrc` (macOS default
shell) or `~/.bashrc`. `clawbridge --version` prints JSON:
`{"source":"build","data":{"version":"…","commit":"…","build_time":"…"}}`. If a download or
checksum fails, the existing binary stays in place. An update replaces the binary
while preserving your saved route, database/cache and native credentials. The
installer never installs an older version over a newer one: it asks the installed
binary its version first, and refuses unless you pass `--allow-downgrade` (Windows:
`-AllowDowngrade`). A binary that doesn't report a release version, such as a source
build, doesn't stop it.

On Windows (x64 or ARM64), download and run the PowerShell installer. It picks the release
asset for the machine's native architecture, `windows_amd64` or `windows_arm64`, even when
PowerShell itself runs emulated (an x64 PowerShell on a Windows on ARM machine installs the
ARM64 build):

```powershell
Invoke-WebRequest -Uri 'https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.ps1' -OutFile "$env:TEMP\clawbridge-install.ps1"
powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\clawbridge-install.ps1"
```

Windows' default execution policy refuses to run a downloaded script, so the
second line bypasses it for that one `powershell` process only; your saved
policy does not change. On Windows PowerShell 5.1, if `Invoke-WebRequest` fails
with a TLS or "secure channel" error, run
`[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12`
first. The installer itself enables TLS 1.2 for its own downloads.

Then open a new terminal and run:

```powershell
clawbridge --version
clawbridge
```

Use `-Version vX.Y.Z` for an explicit binary version or `-InstallDir 'C:\absolute\path'`
for a different directory; add them after the script path. Default is
`%LOCALAPPDATA%\Programs\ClawBridge`. As on macOS and Linux, the installer then runs
`clawbridge skill install` once, a failure only warns, and `-NoSkill` skips it.
Once installed, `clawbridge self-update` upgrades it in place (see [Upgrade](#upgrade)).

Unlike the macOS and Linux installer, the Windows installer adds its directory to
your **user** PATH (`HKCU\Environment\Path`), so new terminals find
`clawbridge`. It appends the directory once and never adds a duplicate: an
existing entry matches regardless of case, a trailing backslash or `%VARIABLES%`.
Other entries and their `%VARIABLES%` are kept unexpanded, and the system PATH is
never touched. When you run the script in your current PowerShell session (with
`&`), it puts the directory first in that session's PATH. Other terminals that
were already open need `$env:Path = '<install directory>;' + $env:Path`, which
the installer prints. If the PATH update fails, the installer warns and the
install still succeeds. Pass `-NoModifyPath` to leave PATH alone; the installer
then prints the manual step. A directory containing `;` cannot be a PATH entry,
so the installer warns and leaves PATH unchanged; run the binary by its full
path or choose another `-InstallDir`.

If `clawbridge` on your PATH is another copy, such as an older install or a
package manager's shim, the installer warns with both paths. New terminals keep
running that copy until you remove it or move the ClawBridge directory ahead of
it in PATH.
To remove the entry later, open *Edit environment variables for your account*
(Start menu, or `rundll32 sysdm.cpl,EditEnvironmentVariables`), select `Path`
under your user variables, then delete the ClawBridge directory.

To update while an agent is running `clawbridge mcp`, rerun the installer. Windows
lets the running binary be renamed, so the new one takes its place, and the old
copy is deleted, or, if it is still running, left as a
`clawbridge-previous-*.exe.old` file in `%TEMP%` (or beside the binary when that
is on another drive) for you to delete later. If Windows refuses the replacement,
the installer stops before reporting success and keeps the old binary. Close the
agents running `clawbridge mcp` and any other `clawbridge` process, then run it
again.

Installer support is Windows PowerShell 5.1 and PowerShell 7 on Windows x64 and ARM64.
On an ARM64 machine the `windows_arm64` binary runs natively; tools that are x64-only still
work alongside it under Windows' x64 emulation. Native
platform runtime acceptance is recorded in the
[release review](archive/reviews/RELEASE-REVIEW.md).

Apart from `skill install`, and asking an already installed binary for its version,
installation does not run ClawBridge, and it never
signs in or changes a ClawBridge configuration. Running `clawbridge` opens your saved
profile or first-run guided setup. Linux/WSL still requires an existing working
Secret Service session for OAuth, as described under Linux and WSL credentials below. No gcloud installation is
needed for browser/device OAuth. Binaries carry build provenance (below) but are not
code-signed; macOS notarization and Windows installer packages are separate future
distribution options.

You can also download the matching raw binary from the release and verify it
against `SHA256SUMS`. The manifest covers five executables, both installers, the
MIT `LICENSE` and the full `SOURCE-COMMIT.txt`. Checksums verify integrity against that release
manifest; they are not a separate publisher signature.

### Verify a download

The release workflow attests the build provenance of every release file (releases
published before it did have no attestation, so the check fails for them): a statement, signed with a short-lived Sigstore certificate for that workflow
run and recorded in a public transparency log, that this repository's
`release.yml` built the file from the tagged commit. A file replaced in the release
afterwards has none. With the [GitHub CLI](https://cli.github.com/):

```sh
gh attestation verify clawbridge_vX.Y.Z_linux_amd64 --repo jkai-chen/ClawBridge \
  --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z
```

The installer is a release file too, so it can be checked before it runs, rather
than piped straight into `sh`:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' -o install.sh \
  https://github.com/jkai-chen/ClawBridge/releases/download/vX.Y.Z/install.sh
gh attestation verify install.sh --repo jkai-chen/ClawBridge \
  --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z
sh install.sh --version vX.Y.Z
```

The installers run the same check when you ask for it: set
`CLAWBRIDGE_VERIFY=required` (`curl … | CLAWBRIDGE_VERIFY=required sh`, or
`$env:CLAWBRIDGE_VERIFY = 'required'` on Windows). Then `gh` must be on PATH and
signed in, and a binary that fails the check is not installed. Without it the
installers check the checksum only. Provenance proves which workflow built a file
from which commit; it does not make that commit safe, which is what review and the
release approval are for.

`clawbridge version` and `clawbridge --version` print JSON with version, commit and
build timestamp without loading app state. Source builds use dev/unknown defaults.

## Upgrade

`clawbridge self-update` (alias `clawbridge upgrade`) replaces the running binary with a
GitHub release, with the installers' checks: the release's `SHA256SUMS` must list the
asset exactly once and the download must match it, and the new binary must report the
release's version before it replaces anything. It keeps your saved route, database,
cache and native credentials, signs in to nothing, and needs no profile.

```sh
clawbridge self-update --check     # reports the running and latest versions; changes nothing
clawbridge self-update             # upgrade to the latest release
clawbridge self-update --version vX.Y.Z
```

- **`--check`** prints the plan as JSON and one line on stderr: an update is available,
  you are up to date, the latest is older than the running version, or this is a
  development build. It exits 0 for all four and reads only the latest-release redirect.
- **Downgrades.** The command never installs an older release by itself. Naming one with
  `--version vX.Y.Z` is the explicit consent; without it, a latest release older than the
  running version is refused. A development build must name its release with `--version`.
- **Attestation.** `--verify-attestation`, or `CLAWBRIDGE_VERIFY=required` as for the
  installers, also runs `gh attestation verify` on the download; `gh` must be on PATH and
  signed in, and a binary that fails is not installed. Without it the output says
  `build provenance: not checked`.
- **The skill.** Afterwards the new binary runs `clawbridge skill install`; a failure only
  warns. `--no-skill` skips it.
- **Agent shells.** Upgrading is a person's decision. When an agent's environment
  (`CLAUDECODE`, `CODEX_*`, `CLAWBRIDGE_SESSION`, `CLAWBRIDGE_INSTANCE`) is set, the
  command refuses without a terminal, so an agent's own tool call changes nothing and
  downloads nothing; at a terminal you type the target version to confirm. In your own
  terminal there is no prompt. `--check` stays allowed everywhere, so an agent can tell
  you an update exists.
- **Where it writes.** The binary it replaces, in place, via a temporary file in the same
  directory. A binary in a directory you cannot write (a package manager's, root-owned)
  is refused with nothing changed; reinstall with the install script, or run the command
  where you can write. It never uses sudo.
- **Windows.** A running `.exe` cannot be overwritten but can be renamed: the old binary
  is renamed aside (`clawbridge-previous-<random>.exe.old`, in `%TEMP%` when that is on the
  same volume, otherwise beside the binary) and the new one takes its name; if the second
  rename fails, the first is undone. The old file stays while any `clawbridge` process
  still runs it, and later runs delete the leftovers.
- **Running agents.** An agent's running MCP server (`clawbridge mcp`) keeps the old
  version until its session restarts. When you use a hosted server, the command also says
  if the server runs an older release than the client now does; `clawbridge doctor`
  compares them.

## Agent skill

The binary carries the ClawBridge agent skill of its own version.
`clawbridge skill install` copies it into the per-user skill directory of each
agent configured on this machine:

| Agent | Skill directory |
| --- | --- |
| Claude Code | `$CLAUDE_CONFIG_DIR/skills/clawbridge`, default `~/.claude/skills/clawbridge` |
| Codex | `$CODEX_HOME/skills/clawbridge`, default `~/.codex/skills/clawbridge` |

An agent counts as configured when its configuration directory exists. With
neither present, the install says "No Claude Code or Codex installation found";
install Claude Code or Codex, then run `clawbridge skill install` again, or pass
`--dir PATH` for a custom agent folder. Use
`--agent claude-code`, `--agent codex` or `--agent all` to choose explicitly, or
`--dir SKILLS_DIR` to install into `SKILLS_DIR/clawbridge` for another agent.

`CLAUDE_CONFIG_DIR` and `CODEX_HOME` decide where the skill goes. A terminal
opened inside an agent or a multi-account tool (for example an Orca agent shell)
often sets them to that account's profile, so a skill install or guided setup
run there targets that profile, not `~/.claude` or `~/.codex`. Run it from a
plain terminal to install for your default agent setup.

Repeating the install changes nothing. After an update, it swaps in a fully
written copy, so an agent never reads a half-written skill (the folder is absent
only between two renames). A `.clawbridge-skill.json` marker records the version
and a hash of every file it wrote. The marker is trusted only when it lists
clean relative paths inside the folder with valid hashes; otherwise it is
reported as corrupt and ignored. A file an older version installed and this one
no longer ships is removed on upgrade while its hash still matches. Nothing is
read or removed through a symbolic link.

Without `--force`, the install refuses and changes nothing when you edited one
of its files, or when the folder holds files it did not install, links, or an
untrusted marker. `--force` never deletes your work: a folder holding edited
files, other files, links or an untrusted marker is first moved, whole, to
`clawbridge-skill-backup-TIMESTAMP` beside the agent's `skills` directory (where
the agent does not load it), and the JSON result reports that path.
`clawbridge skill status` compares the installed version with the binary's, and
`clawbridge doctor` warns when they differ. `clawbridge skill uninstall` removes
only the files the trusted marker lists and keeps any file you added. An edited
file refuses the uninstall; `--force` moves it to such a backup instead of
deleting it. It refuses, even with `--force`, when the marker is untrusted or the
folder holds links.

## Claude Code channel plugin (optional)

To let an idle Claude Code session be woken through a channel, install
ClawBridge's plugin (`clawbridge channel setup`, in a plain terminal) and
allowlist it once per machine. The allow step writes a managed-settings file
that applies to every Claude Code user on the machine, so it needs an
administrator: `sudo clawbridge channel allow --write` on macOS, and on Linux and
WSL, where the file is under `/etc/claude-code/managed-settings.d/` (inside WSL,
for a Claude Code run there); on Windows, `clawbridge channel allow --write` in a
terminal run as administrator, for `C:\Program Files\ClaudeCode\`. It writes
only after a person types `yes` at that terminal; agents never run it. Without the
admin step the notifier keeps waking the session. Read the warnings first:
[wake an idle agent through a channel](notifications.md#wake-an-idle-agent-through-a-channel-claude-code).

## Setting up your own server

`clawbridge onboard` (Start's "Set up my own server") and the
[first-deployment guide](operations/first-deployment.md) cover Google Cloud and
AWS. `clawbridge skill install --playbook gcp` or `--playbook aws` installs that
cloud's provisioning playbook for your agent. Each cloud route uses that cloud's
own CLI, which ClawBridge doesn't install, configure or sign in for you:

| Cloud | CLI | Sign-in | Runs on |
| --- | --- | --- | --- |
| Google Cloud | `gcloud` | a named gcloud configuration | wherever `gcloud` runs |
| AWS | AWS CLI v2; the Session Manager plugin for a shell on the instance | a named profile, for example `aws configure sso --profile clawbridge-onboarding` | macOS, Linux or WSL (not native Windows; see [Windows: set up WSL](#windows-set-up-wsl-for-the-aws-route)) |

ClawBridge never reads `~/.aws` or the gcloud credential store. It checks that
the named profile or configuration exists and is signed in to the account you
chose. The AWS profile's permissions for the checks and `cloud secret set` are
in [`deploy/aws-profile-policy.json`](../deploy/aws-profile-policy.json).

Start's "Where?" question has a third answer, "This machine or my LAN (no
cloud)". It hands `deploy/local-host.sh up` to your agent with the
`--playbook local` skill instead: it needs Docker, no cloud CLI, creates nothing
in any cloud, and runs on macOS, Linux or WSL
([local hosting](operations/local-host.md)).

## Windows: set up WSL for the AWS route

The AWS route needs a Linux shell, so on Windows it runs in WSL (Windows
Subsystem for Linux). Everything below happens inside WSL, not in PowerShell,
and that includes your agent.

1. **Install WSL.** In PowerShell as administrator, run `wsl --install`, then
   restart. Open "Ubuntu" from the Start menu and create your Linux user.
2. **Install ClawBridge inside WSL**, with the macOS and Linux installer at the
   top of this page, not the PowerShell one. The Windows and WSL clients are
   separate installs, each with its own state.
3. **Install the AWS CLI v2 and the Session Manager plugin inside WSL:**

   ```sh
   sudo apt-get update && sudo apt-get install -y unzip
   curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o awscliv2.zip
   unzip -q awscliv2.zip && sudo ./aws/install && rm -rf aws awscliv2.zip
   curl -fsSL https://s3.amazonaws.com/session-manager-downloads/plugin/latest/ubuntu_64bit/session-manager-plugin.deb \
     -o session-manager-plugin.deb
   sudo dpkg -i session-manager-plugin.deb && rm session-manager-plugin.deb
   aws --version && session-manager-plugin --version
   ```

   On an ARM64 Windows machine, use `awscli-exe-linux-aarch64.zip` and the
   `ubuntu_arm64` plugin package instead.
4. **Sign in inside WSL:** `aws configure sso --profile clawbridge-onboarding`.
   It prints a URL and a code; open the URL in your Windows browser. The
   profile lives in WSL's own `~/.aws`, apart from any AWS CLI on Windows.
5. **Run your agent inside WSL too.** The AWS hand-over from `clawbridge onboard`
   and the `--playbook aws` skill run `aws` and `clawbridge` commands, so your
   agent (Claude Code, Codex) has to run in the WSL shell where those commands
   are. Run `clawbridge skill install` in WSL for the agents configured there.

**Where files live.** Your WSL home, `/home/<you>`, holds the binary
(`~/.local/bin`), ClawBridge's state (`~/.clawbridge`) and `~/.aws`. From Windows
Explorer it is under `\\wsl$\Ubuntu\home\<you>`. Keep your work in the WSL home,
not under `/mnt/c`: Windows drives are slow from WSL, and files there can't be
made private to your user, which ClawBridge's state files must be.

**Credentials.** Windows Credential Manager doesn't reach into WSL. With device
sign-in, a keyring is no longer required for anything: a WSL install without a
Secret Service can keep its device key (`join --key-store file`), its Linear
connection (`linear connect --key-store file`) and its Linear API key (`linear key
set --key-store file`) each in a private file, by your explicit choice. Google
sign-in still needs a Secret Service in WSL, as [Linux and WSL
credentials](#linux-and-wsl-credentials) describes.

## Build from source

Requires Go 1.26 or newer. Local mode needs no database server and neither mode
needs a C compiler.

```sh
mkdir -p bin
go build -o bin/clawbridge ./cmd/clawbridge
bin/clawbridge --help
```

## Linux and WSL credentials

OAuth refresh tokens use Linux Secret Service, not plaintext files. The
ClawBridge process and the unlocked keyring must share the same user D-Bus
session. This also applies to WSL and headless SSH: device sign-in changes the
Google consent flow; it does **not** remove the native credential-store
prerequisite. Windows Credential Manager is not automatically bridged into WSL.

Device keys, from [signing in with an invite](sign-in.md) (the client side from v0.3.5), use
the same Secret Service by default, but a machine without one can keep the key in a
0600 file instead (`join --key-store file`). Read
[what that means](sign-in.md#device-keys-the-os-key-store-or-a-file) first.

The personal Linear connection (`linear connect`) does the same: the Secret Service
by default, or with `--key-store file` a 0600 file under `~/.clawbridge/linear/`, which
anyone who can read it as you can use to act as you in Linear. ClawBridge refuses
it on a Windows drive (`/mnt/c`); keep its state in the Linux home. Read [where your
connection is kept](linear.md#where-your-connection-is-kept) first.

To inspect the current session without starting a service or accessing a token:

```sh
busctl --user --timeout=3 call org.freedesktop.DBus /org/freedesktop/DBus \
  org.freedesktop.DBus NameHasOwner s org.freedesktop.secrets
```

`b true` confirms service presence only, not that the keyring is unlocked or
write-ready. If the bus or service is unavailable, restore the existing desktop
or SSH session in which the keyring was available, and run ClawBridge there.
An already-authorized profile can run `whoami` again from that session; do not
rejoin merely to repair the D-Bus environment. ClawBridge does not start services,
provision a keyring, capture a keyring password, or fall back to plaintext storage.

**Which keyring ClawBridge writes to.** ClawBridge saves into the keyring named
`login` when one exists, and only otherwise into the keyring the `default` alias
points to. `secret-tool` uses the `default` alias, so `secret-tool store` can work
while ClawBridge fails, when `login` exists but is locked. A failed save says why
the key store failed, in one of a fixed set of phrases (for example "the login
collection is locked"); nothing the store said, and no credential, is repeated.
To check `login`:

```sh
busctl --user get-property org.freedesktop.secrets \
  /org/freedesktop/secrets/collection/login org.freedesktop.Secret.Collection Locked
```

`b true` means it is locked: unlock it in the session ClawBridge runs in (with
GNOME Keyring, `gnome-keyring-daemon --replace --unlock --components=secrets`
reads its password from standard input), and make sure only one keyring daemon
serves your session. Or connect with `--key-store file` and skip the keyring.
