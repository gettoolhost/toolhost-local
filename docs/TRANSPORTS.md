# Transports

## Front door (clients → toolhost)

| mode | config | wire |
|---|---|---|
| stateless | `"mode": "stateless"` (default) | Streamable HTTP, no `Mcp-Session-Id` — every POST independent |
| stateful | `"mode": "stateful"` | Streamable HTTP with held sessions — pushed `tools/list_changed` reaches clients |
| stdio | `serve --stdio` | MCP over stdin/stdout; no bearer — the spawning process owns the pipe |

- Stateless speaks the 2026-07-28 spec (SEP-2567 sessionless +
  `server/discover` negotiation). Older-protocol clients are served
  sessionlessly — same surface, no held session.
- What stateless can't do: *push*. `tools/list_changed` needs a held
  session. The meta-tools are the pull-based path that works regardless
  of client refresh behavior.
- Bearer auth gates `/mcp` in both HTTP modes; `/healthz` is open.
  DNS-rebinding protection stays on (SDK default). `mode` changes need a
  restart.
- stdio: stdout is protocol-only, status lines go to stderr. Reloads
  still land — `list_changed` travels as a notification.

## Upstream (toolhost → backends)

| `transport` | backend shape |
|---|---|
| `stdio` | local subprocess: `command`, `args`, `env` |
| `http` | Streamable HTTP MCP server (`streamable_http` accepted as alias) |
| `sse` | legacy HTTP+SSE server |

- Each backend's connect+discover handshake is bounded (30s). A dead
  backend contributes zero tools — warned, audited, never silently
  callable.
- `call_timeout` bounds every upstream call (global default `60s`,
  per-backend override, caller's tighter deadline wins).
- Transport-scoped context values never cross the gateway — deadline and
  cancellation propagate to upstream calls; nothing else does.

## Federation

A gateway is itself a Streamable HTTP server — attach it to another
gateway as a backend with `"passthrough": true`:

- Inner qualified names pass through unchanged: `cbm__search_graph`
  stays `cbm__search_graph` at any depth.
- The inner gateway's `toolhost__*` meta-tools are dropped at the edge —
  the outer gateway has its own control plane.
- Governance composes: the outer `approved` list still gates which
  federated tools go live.
