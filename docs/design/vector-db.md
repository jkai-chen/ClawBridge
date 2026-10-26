# No vector database for now

Owner decision, 26 October 2026.

## Context

The owner asked whether agents need a vector database for search or memory.

## Decision

No. ClawBridge adds no vector database or embedding index.

## Why

- **The sources of truth are already readable.** Linear (issues, comments and documents) and the bus store hold
  the work. Agents read them directly through ClawBridge and its MCP tools.
- **The data is small.** Per project it fits in direct reads and filters, without semantic retrieval.
- **It would cost more than it gives.** It means new infrastructure and spend, plus a second copy of Linear
  content that has to be kept in sync and secured like the original.

## Revisit when

- agents need semantic search across many projects; or
- agents need long-term memory beyond Linear and the repository.
