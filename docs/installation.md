# Install and update

**[v0.1.0 is available](https://github.com/jkai-chen/ClawBridge/releases/tag/v0.1.0)**
with versioned binaries, installers and SHA256 checksums. Source builds remain
available below.

For macOS or Linux, the installer selects Intel/AMD64 or ARM64, verifies the
release checksum and installs into `~/.local/bin`:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

To pin the installer and binary version:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/download/v0.1.0/install.sh \
  | sh -s -- --version v0.1.0
```

Use `--bin-dir /absolute/path` to choose another destination. After installing,
the installer runs the new binary once as `clawbridge skill install`, which
installs the [agent skill](#agent-skill) for the agents configured on this machine;
`--no-skill` skips that. A skill failure is a warning; the binary stays installed.
The installer prints the PATH step; it does not change shell profiles or PATH
itself. If a download or
checksum fails, the existing binary stays in place. An update replaces the binary
while preserving your saved route, database/cache and native credentials.

On Windows x64, download and run the PowerShell installer:

```powershell
Invoke-WebRequest -Uri 'https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.ps1' -OutFile "$env:TEMP\clawbridge-install.ps1"
& "$env:TEMP\clawbridge-install.ps1"
$env:Path = "$env:LOCALAPPDATA\Programs\ClawBridge;$env:Path"
clawbridge --version
clawbridge
```

Use `-Version v0.1.0` for an explicit binary version or `-InstallDir 'C:\absolute\path'`
for a different directory. As on macOS and Linux, the installer then runs
`clawbridge skill install` once, a failure only warns, and `-NoSkill` skips it. Default is `%LOCALAPPDATA%\Programs\ClawBridge`.
The PATH command above affects the current session; add the directory to your
user PATH through Windows settings for future terminals. Installer support is
Windows PowerShell 5.1 and PowerShell 7 on Windows x64. Native platform runtime
acceptance is recorded in the [release review](archive/reviews/RELEASE-REVIEW.md).

Apart from `skill install`, installation does not run ClawBridge, and it never
signs in or changes a configuration. Running `clawbridge` opens your saved
profile or first-run guided setup. Linux/WSL still requires an existing working
Secret Service session for OAuth, as described under Linux and WSL credentials below. No gcloud installation is
needed for browser/device OAuth. Binaries are unsigned; macOS notarization and
Windows installer packages are separate future distribution options.

You can also download the matching raw binary from the release and verify it
against `SHA256SUMS`. The manifest covers five executables, both installers and
the full `SOURCE-COMMIT.txt`. Checksums verify integrity against that release
manifest; they are not a separate publisher signature.

`clawbridge version` and `clawbridge --version` print JSON with version, commit and
build timestamp without loading app state. Source builds use dev/unknown defaults.

## Agent skill

The binary carries the ClawBridge agent skill of its own version.
`clawbridge skill install` copies it into the per-user skill directory of each
agent configured on this machine:

| Agent | Skill directory |
| --- | --- |
| Claude Code | `$CLAUDE_CONFIG_DIR/skills/clawbridge`, default `~/.claude/skills/clawbridge` |
| Codex | `$CODEX_HOME/skills/clawbridge`, default `~/.codex/skills/clawbridge` |

An agent counts as configured when its configuration directory exists. Use
`--agent claude-code`, `--agent codex` or `--agent all` to choose explicitly, or
`--dir SKILLS_DIR` to install into `SKILLS_DIR/clawbridge` for another agent.

Repeating the install changes nothing. After an update, it replaces the skill as
a whole, so an agent never reads a half-written skill. A `.clawbridge-skill.json`
marker records the version and a hash of every file it wrote. The install refuses,
and changes nothing, when you edited one of those files or added files of your own
to the folder; `--force` replaces them. `clawbridge skill status` compares the
installed version with the binary's, and `clawbridge doctor` warns when they
differ. `clawbridge skill uninstall` removes only the files the marker lists, and
keeps any file you added.

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
