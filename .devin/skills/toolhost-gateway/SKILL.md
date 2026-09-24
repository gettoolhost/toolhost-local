---
name: toolhost-gateway
description: How to use and operate toolhost, the local governed MCP gateway — one endpoint that fronts many MCP servers behind a three-tier tool gate (discovered/approved/enabled) with an agent-facing control plane. Use this whenever you see toolhost__* tools in your MCP surface, when a needed MCP tool isn't available and a toolhost gateway may be running, when the user mentions toolhost or an "MCP gateway", or when setting up governed MCP access in a project. Agents self-serve the live surface via toolhost__search/enable/call — do NOT ask the human to add MCP servers directly if toolhost is present.
---

# toolhost — the governed MCP gateway

One local HTTP endpoint (`/mcp`, bearer-token auth) that fronts N upstream
MCP servers. Agent-facing tools are namespaced `backend__tool`. The gate:

```
discovered → approved (HUMAN policy) → enabled (live surface)
```

A tool not on the live surface is invisible AND uncallable — by design,
not by accident.

## If `toolhost__*` tools are in your surface

You are attached to a gateway. Self-serve — do not ask the human to edit
MCP config:

| tool | use |
|---|---|
| `toolhost__search` `{query}` | catalog search → qualified names + approved/enabled state |
| `toolhost__list` `{scope}` | `enabled` (default) / `approved` / `all` |
| `toolhost__enable` `{names[]}` | move approved tools onto the live surface NOW |
| `toolhost__disable` `{names[]}` | pull tools off NOW (they stay approved) |
| `toolhost__call` `{name, arguments}` | call any enabled tool by name — works even if your client ignores `tools/list_changed` |

**The loop**: need a tool → `search` → if approved-but-disabled,
`enable` it → call it (directly, or via `toolhost__call` if your list
didn't refresh) → `disable` when done to keep the surface lean.

**Your boundary**: you may move tools between approved and enabled.
You may NOT approve. If `search` shows `approved: false`, the remedy is
to ask the human: `toolhost approve <name>` — never try to route around
it. Every enable/disable/call is written to `toolhost_audit.jsonl`.

## If the gateway is down (connection refused)

It's a local process, not a service:

```bash
# find it: look for toolhost.json in the project root (has "listen" + backends)
./toolhost serve                      # from that dir, or:
tmux new-session -d -s toolhost './toolhost serve'
```

Config file is `toolhost.json` beside the binary; token inside it.
`serve` hot-reloads the file (~2s) — CLI edits apply live.

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
