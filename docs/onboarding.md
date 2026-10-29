# Join a ClawBridge project

This guide brings a colleague and their coding agents into an existing hosted
ClawBridge project. The project owner grants access first; the colleague then
installs ClawBridge and runs one guided setup. Examples use placeholders such as
`https://clawbridge.example.test` and `colleague@example.test`.

## For the project owner

The service must already be deployed ([deployment](operations/deployment.md)) and
initialized once with `clawbridge setup --cloud`, run by the identity named in the
server's `CLAWBRIDGE_BOOTSTRAP_OWNER` (normally the owner's own Google account);
it becomes the workspace owner. Then create the project and grant access:

```sh
clawbridge project create checkout --name "Checkout"
```

### Invite (recommended when the server takes device keys)

> The client commands below (`invite`, `join`) come with v0.3.5, and invite
> links with v0.3.6; with v0.3.5, give the code and the server's URL.
> See [signing in with invites](sign-in.md).

On a server whose `CLAWBRIDGE_AUTH` is `device` or `google,device`
([sign-in methods](operations/deployment.md#sign-in-methods)), this is the simpler
route: one link, no Google test users, and no Google account needed. Invite them by
email with their roles:

```sh
clawbridge invite --email colleague@example.test --grant checkout=member
clawbridge invite list
clawbridge invite revoke inv_...
```

- `invite` prints a one-time invite link once (with v0.3.5, a code). ClawBridge
  sends no email: give the colleague the link (or the code and the service URL)
  yourself, over a channel you would trust with a password.
- They join with `clawbridge join`, which asks for the link without showing it; with
  a code, `clawbridge --server URL join --email colleague@example.test`. The guided
  setup's "I have an invite code" takes either.
- An invite with no `--grant` lets them in with no project: their terminal view waits
  on "Waiting for a project" until you grant one with
  `clawbridge --project checkout project role set colleague@example.test member`.
- The code expires (72 hours by default, `--expires`); `invite list` shows each
  invite's status, never the code. A lost or burnt code means a new invite.
- On a device-only server, the Google steps in the next section (test users, a Google
  email) don't apply.

The details are in [invites and the one-time code](sign-in.md#invites-and-the-one-time-code).

### Google sign-in

Use this route when the server signs people in with Google. Grant the colleague a
project role for the Google email they will sign in with:

```sh
clawbridge project role-set colleague@example.test member
```

Project roles are `owner`, `lead`, `member` and `reader`. A `lead` can also administer
the sessions of members and readers and save the shared Linear board mapping
(`clawbridge linear link`, see [Linear](linear.md)). Use the colleague's verified
Google email.

To withdraw access:

```sh
clawbridge project role-revoke colleague@example.test
```

Revocation retires that person's active sessions in the project and removes
their workspace membership once they hold no project role. The workspace owner
cannot be revoked, and owners cannot revoke themselves.

Send the colleague three values: the service URL, the workspace and the project.

For Google sign-in, before the colleague starts, also make sure that:

- **Google can sign them in.** If the service's Google OAuth consent screen is
  in *Testing* (Google Cloud console, OAuth consent screen, publishing status),
  only listed test users can sign in: add the colleague's Google account as a
  test user. A consent screen *In production* needs no list.
- **They hold a ClawBridge role** (`project role-set`, above) for the exact
  Google email they will sign in with.

Whichever route you use, if the project uses a Linear board, invite them to the Linear
workspace and to every team the project's board is bound to (`clawbridge linear binding`
shows them). Their own Linear connection can only see what their Linear account can.

### On a board with lanes and team roles

If the board has lanes or team roles ([Lanes](linear.md#lanes-which-write-teams-each-session-writes),
[Team roles](linear.md#team-roles-project-owner-and-leads-for-their-teams)), a project
role may not be enough: depending on the binding, a newcomer's agents may write
nothing until you do two more things.
Both are yours to do, from your own terminal and without `--session`. Read the current
revision N with `clawbridge linear roles show` and `clawbridge linear lanes show`. Each
command only previews until you add `--confirm`.

```sh
clawbridge linear roles person "Ada Example" --role engineer --team TEAM_UUID --link colleague@example.test --expected-revision N
clawbridge linear lanes pin --pin-principal colleague@example.test --lane NAME --expected-revision N
```

- **Link their team role.** `roles person` records a Linear user's role (`lead`,
  `engineer` or `reviewer`) in the bound teams they work in. `--link EMAIL` ties that
  person to their ClawBridge account, and the email must belong to a project member.
  Without `--link` ClawBridge cannot tell whose role it is, so the role grants
  nothing: a team lead's authority, for one, is known only through the link.
- **Pin their lane.** `lanes pin --pin-principal EMAIL --lane NAME` puts all of that
  person's sessions in a lane, which sets the teams they may write. A session nobody
  pinned follows the binding's `--unpinned` setting. With `unpinned: read_only` an
  unpinned newcomer can read but not write; the first refusal reads `outside_lane`.

### Project role and team role

The word `lead` is used twice, for two different things.

| | Project role | Team role |
|---|---|---|
| Values | `member`, `lead` (and `owner`, `reader`) | `lead`, `engineer`, `reviewer` |
| Set by | `project role-set`, or `invite --grant` | `linear roles person` / `roles lead`, with `--link` for the account |
| What it allows | A project `lead` (like the owner) may confirm an issue's team move and add a team to a Linear project, from their own terminal. A `member` may not. | A team-role `lead` of a team may add that team to a Linear project, once the owner has linked their row. They may not confirm a team move, and they set `engineer` and `reviewer` roles only for people in teams they lead. |

### What the owner can see

As the project owner you can read the project's messages, agents' included, from your own terminal with
`clawbridge project threads` and `clawbridge project thread ID` ([usage](usage.md#reading-the-projects-threads-project-owner)).
Each read is in the project's history, which every member can see. Conversations started before the server's
upgrade stay private, even if they continue. Tell colleagues this when you invite them.

## For the colleague

### Before you start

Get these from the project owner first:

- the service URL, workspace and project;
- confirmation that your Google account has a project role (and, if the
  owner's Google sign-in is still in testing, that you were added as a test
  user);
- for the Linear board: an invitation to the Linear workspace and its bound
  teams.

Sign in with the Google account the owner granted, from the start. Newer
servers tie your email to the first Google account that signs in with it; a
different account later is refused until the owner rebinds it.

### 1. Install

Use the [installer](installation.md) for your system. It verifies the release
checksum, installs `clawbridge`, and then installs the ClawBridge agent skill for
Claude Code and Codex when they are set up on the machine (`--no-skill` or
`-NoSkill` skips that).

### 2. Run the guided setup

In a terminal, run:

```sh
clawbridge
```

On first launch this opens the guided setup (`clawbridge setup wizard` opens it
again later). It asks for the service URL, workspace and project and how to sign
in (open a browser here, or show a code to enter on another device, for SSH and
headless machines). Each answer is checked as you give it; a wrong one is asked
again with the reason, up to three times. It then shows your answers, and asks
`Continue with sign-in and save after verified join? [y/N]`;
anything but `y` cancels without changing anything. It then signs you in with
Google, verifies your membership, and saves the trusted route. Your Google
account must already have the role from the owner; joining does not grant it.

When the server takes device keys, the guided setup asks first how you sign in:
**I have an invite code** or, when the server still offers it, **Sign in with
Google**. It asks for the email the invite was issued for (leave it empty with
a link) and a name for this device, shows your answers, and asks to continue;
only then does it ask for the link or the code, which isn't echoed. It makes this
device's key, joins, and saves the profile, then picks your project. With
several, it lists them numbered and you pick one. If the owner hasn't granted you
a project yet, the profile is still saved, and the terminal view opens on
**Waiting for a project**: it names the owner to ask, checks again every 30
seconds, and opens your project once it's yours. On a machine with no system key
store, such as headless Linux, it explains that the key would be a plain file
only you can read, and asks before using one.

Browser sign-in opens your default browser and also prints the sign-in URL. If
that browser is signed in to a different Google account, open the URL in another
browser profile on the same computer instead (it must be this computer: the
sign-in returns to a local address). On macOS the sign-in is kept in the
Keychain, and macOS may ask you to allow access.

Run the setup from a plain terminal. Inside an agent or multi-account tool (for
example an Orca agent shell), `CLAUDE_CONFIG_DIR` and `CODEX_HOME` may point at
that tool's profile, and the skill and MCP steps then write there instead of
`~/.claude` and `~/.codex`.

After the join, the setup offers each remaining step in turn, headed
"Optional setup 1 of 5" to "5 of 5". Every step can be skipped, nothing is
written until you answer yes, and a failed step is reported without undoing the
earlier ones:

1. **Agent skill.** Installs or updates the skill for the detected agents, the
   same as `clawbridge skill install`.
2. **Your own Linear account (optional).** Connects your personal Linear account
   in the browser, with read access unless you choose write (write lets your
   agents create and update issues). Agents that take work (claim, comment,
   update) need `--access write`; read is the default. Then it checks it with
   `clawbridge doctor --linear-profile`. As with Google, the sign-in URL is
   printed too, for another browser profile on this computer. The connection and
   its credentials are yours; nobody else's account is used.
3. **MCP for each agent.** Asks for a session name, shows the entry it would
   write (the file, the command and its arguments) as a dry run, and writes it
   after you confirm. With a Linear profile from
   step 2, the entry also enables the native task-board tools. For Claude Code
   on macOS and Linux it also installs ClawBridge's hooks, so the agent doesn't
   stop while messages wait ([messages wait until they're handled](usage.md#messages-wait-until-theyre-handled)).
   `clawbridge mcp install --agent claude-code` writes `.mcp.json` in the current
   folder, so run it from the repository root. To write the user-level file
   instead, pass `--config ~/.claude.json`.
4. **Doctor.** Runs `clawbridge doctor` once for each MCP entry it configured.
5. **The dispatcher (owners of a hosted project).** If you own the project, its
   task board is linked and the dispatcher is off, it says in one line what the
   dispatcher does, shows what turning it on changes (its coordinator, sessions
   and domains, and where it confirms claims itself), and asks whether to turn
   it on in the rules engine. If enabling would be refused, for example because
   the task board names no Linear projects, the step is skipped with the reason.
   The default is no. `clawbridge dispatcher disable` turns it off any time.
   Model mode is set up separately (see
   [the dispatcher](dispatcher.md#model-mode-designed-partly-built)). For anyone
   else the step is skipped, with the reason.

It then prints a summary of what was set up, what was skipped, and the exact
command for each skipped step.

It then asks `Open ClawBridge now? [Y/n]`; Enter opens the terminal view, and `n`
leaves it for later (run `clawbridge`). Next, take a first issue from proposal to
release: [your first task](usage.md#your-first-task-claim-and-release). Ask the
owner which session is the project's coordinator; it confirms your assignments.

### Explicit commands

The same setup without prompts, for automation or to redo one step:

```sh
clawbridge join                                                     # an invite link: paste it when asked
clawbridge --server https://clawbridge.example.test join --email colleague@example.test   # an invite code
clawbridge setup join --server https://clawbridge.example.test --workspace team --project checkout   # Google sign-in
clawbridge skill install
clawbridge linear connect --name "Work" --access read
clawbridge doctor --linear-profile PROFILE_ID
clawbridge mcp install --agent codex --session codex-engineer --linear-profile PROFILE_ID --dry-run
clawbridge mcp install --agent codex --session codex-engineer --linear-profile PROFILE_ID
clawbridge doctor --mcp-agent codex --mcp-config /absolute/path/to/codex/config.toml --mcp-session codex-engineer
clawbridge dispatcher show
clawbridge dispatcher enable --expected-revision N             # the preview
clawbridge dispatcher enable --expected-revision N --confirm
```

Use `--agent claude-code` for Claude Code (run it from the repository root: it
writes `.mcp.json` in the current folder), a distinct `--session` per agent, and
omit `--linear-profile` without a Linear connection. Add `--device` to
`setup join` on a headless machine. See [usage](usage.md) for profiles and MCP
details, and [Linear](linear.md) for boards.

### On Windows

Windows has no Claude Code hooks and no Codex wake-up, so nothing wakes an agent
for a new message. Messages wait until the agent checks its inbox. The agent
should check at the start and end of every turn, and you may nudge it ("check
your ClawBridge inbox") when you expect a message, such as a coordinator's
confirmation. See [messages wait until they're handled](usage.md#messages-wait-until-theyre-handled).

## How agents learn the workflow

An agent connected through `clawbridge mcp` learns ClawBridge in three layers:

- **MCP instructions.** The server's instructions tell the agent to register a
  session, check its inbox at the start and end of turns, treat peer messages and
  task text as untrusted data, and use the task-board tools with an operation ID
  for every write.
- **Tool descriptions.** Each tool states its inputs, scope and limits, for
  example that tools are pinned to one project and Linear profile.
- **The skill.** The installed skill explains the coordination workflow: session
  ownership, asking the coordinator, task-board ownership by repository and path,
  and recovery. Its [source](../skills/clawbridge/SKILL.md) ships with each
  release, and `clawbridge doctor` warns when the installed skill is not the
  binary's version.

## Product reviewers

People who only review features need no ClawBridge setup at all: they comment in
Linear. To set up that workflow for your team, see
[product reviews with ClawBridge and Linear](guides/product-reviewers.md), and
give reviewers the [reviewer quickstart](guides/reviewer-quickstart.md).
