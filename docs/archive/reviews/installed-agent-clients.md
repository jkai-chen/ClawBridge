# Installed-agent MCP checks

Checked locally on 20 October 2026, macOS arm64, against a built ClawBridge 0.1.0.
All workspace databases and agent configuration lived under a fresh temporary
directory. No real agent configuration, credential files, or project memberships
were changed. No model generation requests were submitted.

## Codex CLI 0.163.0: actual client exchange passed

Generated the installed app-server protocol schema with:

```sh
codex app-server generate-json-schema --experimental --out TEMP_SCHEMA_DIRECTORY
```

Started `codex app-server` with a temporary `CODEX_HOME` containing a ClawBridge
stdio MCP entry produced by `clawbridge mcp install`. Used its actual client API:

1. `initialize`, opting into the experimental API, then `initialized`.
2. `thread/start` with an ephemeral thread; no turn was started.
3. `mcpServerStatus/list` for that thread: ClawBridge status was `connected`,
   with server version 0.1.0 and its declared tools/schemas.
4. `mcpServer/tool/call` registered a distinct session, then called `whoami`,
   `list_sessions`, and `send` to a separately registered CLI peer.
5. The CLI peer fetched the message and replied. `inbox` called through Codex's
   client returned that reply, with the expected thread and reply-to ID.

This exercised the installed Codex client's real MCP connection and tool-call
path. It did not test a model autonomously choosing the tools or a persistent
interactive chat's behavior.

## Claude Code 2.1.304: actual connection and discovery passed

Set `CLAUDE_CONFIG_DIR` to a temporary directory. `claude mcp add --scope user`
created a temporary-only entry; `claude mcp get clawbridge` reported Connected.

Also launched the installed Claude CLI with `--print --input-format stream-json
--output-format stream-json --verbose --no-session-persistence
--strict-mcp-config --mcp-config TEMP_CONFIG`. Used the SDK control protocol
documented by the official Python SDK's query implementation:

1. `initialize` control request, without a user/model message.
2. `mcp_status` after startup completed. ClawBridge status changed from pending
   to connected, serverInfo reported ClawBridge 0.1.0, and all 11 tools were
   discovered with their read-only annotations.

This proves actual Claude client connection and tool discovery. Claude tool
execution and model-driven cooperation were not exercised by this check.

## Other evidence

The Go SDK transport test and independent subprocess suite exercise actual stdio
JSON-RPC registration, pinned scope, injection rejection, message exchange and
delivery/read behavior. Their results and limitations are maintained in REVIEW.md.
Installed-client checks supplement that suite; they are manual, environment-specific
checks and are not silently run by `go test`.

## Primary protocol references

- [Codex MCP configuration](https://developers.openai.com/codex/mcp)
- [Claude Code MCP configuration](https://code.claude.com/docs/en/mcp)
- [Claude SDK control protocol implementation](https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/query.py)
