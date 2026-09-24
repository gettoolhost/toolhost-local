# Architecture

Ports & adapters. The domain core knows nothing about files, sockets, or
subprocesses; adapters own those. One dependency direction: adapters → core.

```
                       driving                          driven
┌──────────┐   ┌──────────────┐   ┌──────────┐   ┌─────────────────┐
│ MCP       │   │  frontdoor    │   │  core     │   │ upstream         │
│ client ───┼──▶│ (HTTP+bearer, │──▶│ Resolve,  │──▶│ (mcp client:     │
│ /mcp      │   │  go-sdk srv)  │   │ dispatch) │   │  stdio + http)   │
└──────────┘   └──────────────┘   │           │   └─────────────────┘
               ┌──────────────┐   │           │   ┌─────────────────┐
│ toolhost CLI │   │  app          │──▶│           │──▶│ audit            │
│ (cmd/)       │   │  (use cases)  │   │           │   │ (JSONL sink)     │
└──────────┘   └──────────────┘   └──────────┘   │ config           │
                                                  │ (JSON file)      │
                                                  └─────────────────┘
```

## Packages

| Path | Role |
|---|---|
| `internal/core` | Domain + ports. `Upstream`, `AuditSink` interfaces; `Resolve` — the one place visibility is decided. |
| `internal/namespace` | `backend__tool` grammar. Pure functions. |
| `internal/config` | Driven adapter: JSON config load/save, approve/revoke. |
| `internal/upstream` | Driven adapter: `core.Upstream` over go-sdk client sessions (stdio via `CommandTransport`, http via `StreamableClientTransport`). |
| `internal/audit` | Driven adapter: `core.AuditSink` appending JSONL. |
| `internal/frontdoor` | Driving adapter: `/mcp` HTTP server — bearer auth, go-sdk `mcp.Server`, registers exactly what `core.Resolve` returns. |
| `internal/app` | Use cases: `Init`, `Discover`, `Approve`, `Serve` orchestration. |
| `cmd/toolhost` | Flag parsing only. |

## Decisions that are load-bearing

- **The MCP protocol model is the domain vocabulary.** Ports speak
  `*mcp.Tool`, `json.RawMessage` arguments, `*mcp.CallToolResult`. This is an
  MCP gateway; a translation layer would add code, not clarity.
- **One resolver.** `core.Resolve(discovered, approved)` is the only place
  "what can be seen or called" is answered. The front door registers exactly
  that set; dispatch is a closure per qualified name — invisible is
  uncallable *structurally*, not by a check that could be skipped.
- **`__` separator.** See `internal/namespace` — chosen because MCP clients
  enforce `^[a-zA-Z0-9_-]{1,64}$` on tool names; `::` and `.`/`/` fail it.
- **Approval is a file.** `approved` is a list of qualified names in
  `toolhost.json`. `toolhost approve` edits it; `serve` reads it at boot.
  No live mutation yet — restart to apply.
- **Fail closed.** A backend that won't connect contributes zero tools
  (warned, audited `backend_error`, never silently callable). A tool whose
  name can't be safely namespaced is skipped. A duplicate qualified name is
  a hard error, not a coin flip.
- **Auth is one bearer token**, constant-time compared. Deliberately boring.

## Deliberately absent

OAuth/AS facade, per-principal upstream credentials, tenancy, policy beyond
the approve-list, rate limiting, schema pinning, generations/hot-reload,
console, portal, Postgres. The `reference/` tree has production-grade
versions of all of it — port when a real need appears, not before.
