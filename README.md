# Toolhost

One governed front door between your agent and your MCP servers.

You run MCP servers. Toolhost sits in front of them: it discovers every tool
they expose, you approve the subset your agent may use, and only those tools
are visible and callable on a single `/mcp` endpoint — with a JSONL record of
every call.

**discovered ≠ approved ≠ enabled.** Plumbing is easy; this is the part that
makes it safe.

## Install

```bash
go install github.com/gettoolhost/toolhost-local/cmd/toolhost@latest
# or from source: go build -o toolhost ./cmd/toolhost
```

## Quickstart

```bash
./toolhost init                 # writes toolhost.json with a fresh bearer token
$EDITOR toolhost.json           # add your backends (stdio or http)
./toolhost discover             # every tool your backends expose, qualified
./toolhost approve fs__read_file fs__list_directory
./toolhost serve                # http://127.0.0.1:8080/mcp — Authorization: Bearer <token>
cat toolhost_audit.jsonl        # every call, identified and timed
```

Config — three transports × four upstream auth modes:

```json
{
  "listen": "127.0.0.1:8080",
  "token": "th_…",
  "audit_log": "toolhost_audit.jsonl",
  "backends": {
    "fs":      { "transport": "stdio", "command": "npx",
                 "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"] },
    "remote":  { "transport": "http", "url": "https://mcp.example.com/mcp" },
    "keyed":   { "transport": "http", "url": "https://mcp.example.com/mcp",
                 "auth": { "type": "bearer", "token": "sk-…" } },
    "machine": { "transport": "http", "url": "https://mcp.example.com/mcp",
                 "auth": { "type": "client_credentials",
                           "client_id": "…", "client_secret": "…" } },
    "human":   { "transport": "http", "url": "https://mcp.example.com/mcp",
                 "auth": { "type": "oauth" } },
    "legacy":  { "transport": "sse", "url": "https://mcp.example.com/sse" }
  },
  "approved": ["fs__read_file"]
}
```

| transport | none | `bearer` | `client_credentials` | `oauth` |
|---|---|---|---|---|
| `stdio` | ✓ | via `env` | — | — |
| `http` | ✓ | ✓ | ✓ | ✓ |
| `sse` | ✓ | ✓ | ✓ | ✓ via stored grant |

Credential fields take `env:` references so `toolhost.json` is committable —
`"token": "env:TOOLHOST_TOKEN"`, a header value, `auth.token`,
`auth.client_id`, `auth.client_secret`. They resolve at use, never persist
resolved values, and a missing/empty var fails closed at load.

`stdio` backends take credentials through `env`. `bearer` is the API-key
shape — also expressible as a raw `headers` entry. `client_credentials`
discovers the token endpoint from the server's Protected Resource Metadata
and mints on connect. `oauth` is auth-code + PKCE:

```bash
./toolhost auth human      # browser + loopback callback; grant is persisted
./toolhost serve           # reuses and refreshes it — never opens a browser
```

Grants live in `toolhost_tokens.json` next to the config (0600). Without a
grant, `serve`/`discover` fail that backend with "run `toolhost auth`" —
no surprise browser.

**Federation**: a gateway is itself a Streamable HTTP MCP server — attach
it to another gateway with `"passthrough": true` on that backend. Its
already-qualified tool names pass through unchanged
(`cbm__search_graph` stays `cbm__search_graph` at any depth), and its
`toolhost__*` control plane is dropped at the edge.

Point any MCP client at `http://127.0.0.1:8080/mcp` with the bearer token.
Tools are namespaced `backend__tool`.

## Transport modes — stateless first

```json
{ "mode": "stateless" }   // default — you usually don't write this
{ "mode": "stateful" }    // opt-in: session-bearing, for push-capable clients
```

`serve --stdio` speaks the same governed surface over stdin/stdout instead
of HTTP — for clients that only spawn subprocesses (Claude Desktop-class
hosts). No bearer token: the spawning process owns the pipe. Status lines
go to stderr; stdout is protocol-only.

**Stateless is the primary design** (SEP-2567 sessionless Streamable HTTP,
speaking the 2026-07-28 protocol): no `Mcp-Session-Id`, every request
independent — initialize, `tools/list`, `tools/call` each stand alone.
Clients speaking the new spec negotiate via `server/discover`; older
protocol versions are served sessionlessly too — they get the same surface,
just no held session.

The one thing stateless can't do is *push*: `tools/list_changed` needs a
held session. Agents don't miss it — `toolhost__list`/`search`/`call` are
the pull-based path that works regardless of client refresh behavior.

`"mode": "stateful"` is the compat opt-in for clients that must hold
`Mcp-Session-Id` to receive pushed list-changes. Same governance, same
auth, same surface — it only adds session state.

Either way: bearer auth gates `/mcp` before MCP handling, and the SDK's
DNS-rebinding protection stays on. Changing `mode` needs a restart.

`call_timeout` bounds every upstream call (default `"60s"`, Go duration
syntax). A per-backend `call_timeout` overrides it; a caller's tighter
deadline still wins. A hung backend can never hold a call forever — and
the default hot-reloads for already-connected sessions.

`serve` watches the config file (~1.5s poll): edit `approved`, `enabled`,
or `backends` — via CLI or by hand — and the live surface swaps in place.
Stateful clients get `tools/list_changed`; unchanged backend sessions are
kept, changed/removed ones re-dialed or closed. `listen`, `token`,
`audit_log`, and `mode` still need a restart. An invalid save keeps the
current surface (audited `reload` with the error).

## The agent-facing control plane

The gateway also serves its own tools under the reserved `toolhost__`
namespace — always present, unaffected by reloads:

| tool | what it does |
|---|---|
| `toolhost__search` | find tools across the whole catalog by name/description, with approved/enabled state |
| `toolhost__list` | `enabled` (default), `approved`, or `all` |
| `toolhost__call` | call any *enabled* tool by name — the escape hatch for clients that don't refresh on `tools/list_changed` |
| `toolhost__enable` | move approved tools onto the live surface, right now |
| `toolhost__disable` | pull tools off the live surface, right now |
| `toolhost__request` | file an approval request for unapproved tools — queued in `requested`, answered by `toolhost approve` |
| `toolhost__status` | gateway health: mode, per-backend state, counts, pending requests |

So the agent can self-serve: search the catalog → enable what it needs →
call it → disable when done. The trust split holds: **agents govern
`enabled`; humans govern `approved`.** Enable refuses unapproved tools —
approval stays a human decision (`toolhost approve`). Every agent-side
enable/disable is audited (`govern` events) and refused calls are too
(`tool_forbidden`).

The request flow closes the loop without weakening it: `toolhost__request`
writes `requested` entries (name + reason, deduped) — visible in
`toolhost status` and `toolhost__status`. Requesting approves nothing;
`toolhost approve <name>` answers the ask and consumes the queue entry.

```bash
./toolhost status     # live: mode, backends up/down, counts, pending asks
                      # down: prints what the config knows, incl. grants
./toolhost install    # serve as a service — launchd (macOS) / systemd user
./toolhost uninstall  # stop and remove it
```

## What this is not (yet)

No tenancy, no console, no database, no upstream credential brokerage
beyond the auth block above. A single key, a single endpoint, a governed
call.

## Layout

`docs/ARCHITECTURE.md` is the one-page map: ports & adapters, the resolver
that owns visibility, and what lands where.
