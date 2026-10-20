# CLI binding and resumption

Run `clawbridge --help` for installed command details. Commands return JSON with
`source` and `data`; failures use stderr and a nonzero exit status.

Supply `--db`, `--workspace`, `--project`, `--session`, and `--instance`, or use the
corresponding `CLAWBRIDGE_*` environment variables. Repository scope can be set in
`.clawbridge.toml`; explicit selection takes precedence. Resolve a missing or
ambiguous project with the human's known project configuration rather than guessing.

Register a new, uniquely named session and record its returned instance:

```sh
clawbridge --workspace team --project app --session reviewer register --agent-kind codex --role reviewer
```

Use that instance for later CLI commands. To explicitly take over an active
session, use `register --resume --instance PRIOR_INSTANCE` and record the new
instance. This fences the old instance; ordinary repeated registration using the
same instance and metadata is idempotent. A conflicting or
retired registration requires a distinct replacement name; it is not permission
to overwrite the old session.

```sh
clawbridge --workspace team --project app --session reviewer --instance INSTANCE whoami
clawbridge --workspace team --project app --session reviewer --instance INSTANCE inbox
clawbridge --workspace team --project app --session reviewer --instance INSTANCE send lead 'Review ready; see REVIEW.md' --operation-id review-ready-1
```

For MCP installation, `clawbridge mcp install --help` explains supported agents,
pinned scope, explicit config paths and dry-run output. Install each agent session
under a distinct entry name if configuring several entries in the same file.
Installation never starts an agent or grants project membership.
