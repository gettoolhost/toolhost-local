# Architecture

Ports & adapters. The domain core knows nothing about files, sockets, or
subprocesses; adapters own those. One dependency direction: adapters → core.

```
                       driving                          driven
┌──────────┐   ┌──────────────┐   ┌──────────┐   ┌─────────────────┐
│ MCP       │   │  frontdoor    │   │  core     │   │ upstream         │
│ client ───┼──▶│ (HTTP+bearer, │──▶│ Resolve,  │──▶│ (mcp client:     │
│ /mcp      │   │  go-sdk srv)  │   │ dispatch) │   │  stdio/http/sse) │
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
| `internal/upstream` | Driven adapter: `core.Upstream` over go-sdk client sessions (stdio via `CommandTransport`, http via `StreamableClientTransport`, sse via `SSEClientTransport`); assembles each backend's upstream auth. `Call` detaches the request ctx — deadline/cancel cross, values never do. |
| `internal/oauth` | Driven adapter: upstream OAuth — SDK `OAuthHandler` construction (client-credentials, auth-code+PKCE), loopback callback fetcher, `toolhost_tokens.json` grant store. |
| `internal/audit` | Driven adapter: `core.AuditSink` appending JSONL. |
| `internal/frontdoor` | Driving adapter: `/mcp` HTTP server — bearer auth, go-sdk `mcp.Server`, registers exactly what `core.Resolve` returns. |
| `internal/app` | Use cases: `Init`, `Discover`, `Approve`, `Auth`, `Serve` orchestration. |
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
- **Approval is a file.** `approved`/`enabled` are lists of qualified
  names in `toolhost.json`. `serve` polls the file (~1.5s) and hot-swaps
  the surface: `frontdoor.Reload` diffs the live set — unchanged backend
  sessions are kept, changed/removed ones re-dialed or closed — then
  `AddTool`/`RemoveTools` mutate the MCP server, which emits
  `tools/list_changed` (reachable by session-bearing clients only — see
  modes). Config saves are atomic (tmp+rename) so the
  watcher never reads a torn write; an invalid or unresolvable config
  keeps the current surface and audits the failure. `listen`, `token`,
  `audit_log`, `mode` still require a restart.
- **Stateless is the primary transport.** `mode` defaults to
  `"stateless"` (SEP-2567): no `Mcp-Session-Id`, every POST independent.
  Clients on the 2026-07-28 spec negotiate via `server/discover`; older
  protocol versions are served sessionlessly — initialize/list/call each
  stand alone, they just can't receive pushed `tools/list_changed` (the
  meta-tools are the pull path). `"stateful"` opts back into held sessions
  for exactly that push. Bearer auth and DNS-rebinding protection apply in
  both modes.
- **Transport values never cross the gateway.** The SDK stores the
  downstream client's negotiated protocol version in the request ctx;
  `upstream.Call` detaches to a ctx carrying only deadline+cancel —
  otherwise a new-protocol downstream would stamp 2026-07-28 onto calls
  to a legacy upstream and fail as Bad Request. Lifetime propagates;
  transport-scoped values do not.
- **The gateway is a tool too.** `frontdoor` registers five meta-tools —
  `toolhost__{search,list,call,enable,disable}` — outside the resolved
  set, so `Reload` can never drop them. They call into `liveSet`: search
  and list read the discovered catalog plus governance state; enable and
  disable write through the same config file the CLI edits (approval gate
  included) then reload synchronously; `call` dispatches through the live
  set at call time — the escape hatch for clients that ignore
  `tools/list_changed`. Agents govern `enabled`; humans govern
  `approved`. Agent-initiated governance is audited as `govern` events;
  refused calls as `tool_forbidden`.
- **Fail closed.** A backend that won't connect contributes zero tools
  (warned, audited `backend_error`, never silently callable). A tool whose
  name can't be safely namespaced is skipped. A duplicate qualified name is
  a hard error, not a coin flip. A missing OAuth grant fails with the remedy
  ("run `toolhost auth`") — `serve` never opens a browser.
- **Two auth directions, kept apart.** Downstream: one bearer token,
  constant-time compared — deliberately boring. Upstream: per-backend
  `auth` block (none/bearer/client_credentials/oauth); grants persist to
  `toolhost_tokens.json` beside the config, never inside it.

## Deliberately absent

An OAuth authorization server of our own, per-principal upstream
credentials, tenancy, policy beyond the approve-list, rate limiting, schema
pinning, config generations, console, portal, Postgres. The
`reference/` tree has production-grade versions of all of it — port when a
real need appears, not before.
