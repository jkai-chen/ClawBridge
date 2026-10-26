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
while preserving your saved route, database/cache and native credentials.

On Windows x64, download and run the PowerShell installer:

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

Installer support is Windows PowerShell 5.1 and PowerShell 7 on Windows x64. Native
platform runtime acceptance is recorded in the
[release review](archive/reviews/RELEASE-REVIEW.md).

Apart from `skill install`, installation does not run ClawBridge, and it never
signs in or changes a ClawBridge configuration. Running `clawbridge` opens your saved
profile or first-run guided setup. Linux/WSL still requires an existing working
Secret Service session for OAuth, as described under Linux and WSL credentials below. No gcloud installation is
needed for browser/device OAuth. Binaries are unsigned; macOS notarization and
Windows installer packages are separate future distribution options.

You can also download the matching raw binary from the release and verify it
against `SHA256SUMS`. The manifest covers five executables, both installers, the
MIT `LICENSE` and the full `SOURCE-COMMIT.txt`. Checksums verify integrity against that release
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
