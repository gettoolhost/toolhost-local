---
name: toolhost-gateway
description: How to use and operate toolhost — the local governed MCP gateway, one bearer-gated /mcp endpoint that fronts many MCP servers behind a three-tier tool gate (discovered/approved/enabled) with an agent-facing control plane. Use this whenever you see toolhost__* tools in your MCP surface, when a needed MCP tool isn't available and a toolhost-local gateway may be running, when the user mentions toolhost-local or an "MCP local gateway", or when setting up governed MCP access in a project. Agents self-serve the live surface via toolhost__search/enable/call — do NOT ask the human to add MCP servers directly if toolhost is present.
---

# toolhost-local — the governed MCP gateway

toolhost is a gateway brand: this is the **local edition** — a single
binary that fronts N upstream MCP servers behind one bearer-gated
`/mcp` endpoint. It IS a standard Streamable HTTP MCP server, so it
composes: attach a local gateway to another gateway (or any MCP client)
as one upstream — federation is a config entry, not a feature.

Agent-facing tools are namespaced `backend__tool`. The gate:

```
discovered → approved (HUMAN policy) → enabled (live surface)
```

A tool not on the live surface is invisible AND uncallable — by design,
not by accident.

The gateway is **stateless-first** (`"mode": "stateless"`, the default —
SEP-2567, 2026-07-28 spec): no `Mcp-Session-Id`, every request
independent. Legacy-protocol clients are served sessionlessly too. What
they can't receive is *pushed* `tools/list_changed` — irrelevant to you:
`toolhost__list`/`search`/`call` are pull-based and always current. Only
if a client *must* hold a session for push does `"mode": "stateful"`
exist — it's a config edit + restart, ask the human.

## If `toolhost__*` tools are in your surface

You are attached to a gateway. Self-serve — do not ask the human to edit
MCP config:

| tool | use |
|---|---|
| `toolhost__search` `{query}` | catalog search → qualified names + approved/enabled/requested state |
| `toolhost__list` `{scope}` | `enabled` (default) / `approved` / `all` |
| `toolhost__enable` `{names[]}` | move approved tools onto the live surface NOW |
| `toolhost__disable` `{names[]}` | pull tools off NOW (they stay approved) |
| `toolhost__call` `{name, arguments}` | call any enabled tool by name — works even if your client ignores `tools/list_changed` |
| `toolhost__request` `{names[], reason}` | file an approval ask — queued in `requested`, a human's `toolhost approve` answers it |
| `toolhost__status` | read-only gateway health: mode, backends, counts, pending asks |

**The loop**: need a tool → `search` → if approved-but-disabled,
`enable` it → call it (directly, or via `toolhost__call` if your list
didn't refresh) → `disable` when done to keep the surface lean.

**Your boundary**: you may move tools between approved and enabled.
You may NOT approve. If `search` shows `approved: false`, file
`toolhost__request` with a reason — it lands in `requested` where the
human sees it (`toolhost status`) — then ask the human to run
`toolhost approve <name>`. A request approves nothing by itself;
never try to route around it. Every enable/disable/call/request is
written to `toolhost_audit.jsonl`.

## If the gateway is down (connection refused)

It's a local process — check first, restart if needed:

```bash
# find it: toolhost.json sits beside the binary (has "listen" + backends)
./toolhost status                     # up → live report; down → config view
./toolhost serve                      # from that dir, or install it once:
./toolhost install                    # launchd (macOS) / systemd --user (linux)
```

`serve` hot-reloads the config file (~2s) — CLI edits apply live.

`serve --stdio` runs the same governed surface over stdin/stdout — for
clients that only spawn subprocesses. No bearer (the pipe's owner is the
authority); status lines go to stderr, stdout is protocol-only.

## Composing gateways (federation)

A local gateway is an attachable upstream. In another gateway's
`toolhost.json`:

```json
"edge": {
  "transport": "http",
  "url": "http://127.0.0.1:8188/mcp",
  "auth": { "type": "bearer", "token": "th_<the local gateway's token>" },
  "passthrough": true
}
```

`passthrough: true` says "this upstream is a toolhost gateway — its names
arrive already qualified (`cbm__search_graph`), trust them as-is instead
of re-namespacing." Qualified names are stable through any depth of
gateways. The inner gateway's own `toolhost__*` control plane is dropped
at the edge (logged as skipped) — each gateway keeps its own. This is the
path to the enterprise tier: same protocol, bigger door.

## Setting up toolhost in a new project

```bash
go build -o toolhost ./cmd/toolhost   # source: the mcp-gateway repo
./toolhost init                       # writes toolhost.json + bearer token
# edit backends: { "name": {"transport":"stdio","command":...} }
#                { "name": {"transport":"http","url":..., "auth":{...}} }
./toolhost discover                   # list every upstream tool
./toolhost approve <name__tool> ...   # HUMAN policy — what may ever be used
./toolhost enable [--only] <names>    # optional: shrink the live surface
./toolhost serve                      # http://127.0.0.1:8188/mcp
```

Upstream auth: `bearer` token, `client_credentials`, or interactive
`oauth` (`./toolhost auth <backend>` — browser PKCE, grant persists in
`toolhost_tokens.json`; `serve` refreshes silently). `stdio` backends
take creds via `env` instead of `auth`.

## Attaching any MCP client

Register ONE server — the gateway — instead of N:

```
transport: http (streamable)
url:       http://127.0.0.1:8188/mcp
header:    Authorization: Bearer <token from toolhost.json>
```

| client | command |
|---|---|
| devin  | `devin mcp add toolhost --transport http http://127.0.0.1:8188/mcp -H "Authorization: Bearer <tok>"` |
| claude | `claude mcp add toolhost --transport http http://127.0.0.1:8188/mcp -H "Authorization: Bearer <tok>"` |
| others | same shape in their JSON config (`"type":"http"` + `headers`) |

## Invariants worth remembering

- Ambiguity denies: malformed names skip, dead backends contribute zero
  tools, invalid config keeps the last good surface.
- `approved` is human policy; agent enable/disable is audited `govern`,
  refused calls audited `tool_forbidden`.
- The gateway's own `toolhost__*` tools can't be disabled or removed —
  the control plane always survives.

## Feedback — leave a trail

Hit a bug, a confusing error, a workaround you invented, or a gap in
this skill? Don't patch the skill — drop one file in `feedback/`
(beside this SKILL.md): `YYYY-MM-DD-<slug>.md`. What happened, what you
expected, what would have helped. Three lines beats an essay. The
maintainer harvests these manually — the more the product is used, the
better it gets.
