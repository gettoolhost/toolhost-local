# Toolhost

[![CI](https://github.com/gettoolhost/toolhost-local/actions/workflows/ci.yml/badge.svg)](https://github.com/gettoolhost/toolhost-local/actions/workflows/ci.yml)
![Go 1.26.2](https://img.shields.io/badge/Go-1.26.2-00ADD8)
[![MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/gettoolhost/toolhost-local?label=release)](https://github.com/gettoolhost/toolhost-local/releases)

**A Local MCP Gateway: every MCP server behind one endpoint, only the tools you approve on it.**

Toolhost connects AI agents to your MCP servers through one bearer-gated `/mcp`. You register servers once — with the same `mcp add` grammar every client CLI already taught you — approve the tools an agent may use, and the agent self-serves inside that envelope. Every governed call adds a JSONL audit record.

![Toolhost: one front door, only approved tools](assets/toolhost-cover.svg)

## Quickstart

From a source checkout with Go 1.26.2. This example backend uses Node.js/`npx`.

```bash
go build -o toolhost ./cmd/toolhost
./toolhost init
# wrote ~/.toolhost/toolhost.json — a private env for this folder is
# `./toolhost init --project`; a committable ./.toolhost/toolhost.json is
# `./toolhost init --shared`
./toolhost mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp
# added backend fs (stdio) — the `X mcp add` grammar, pointed at one registry
./toolhost discover
#   fs__read_file  <upstream description>
./toolhost approve fs__read_file
# approved fs__read_file — schema-pinned to the contract you just reviewed
./toolhost serve
# toolhost <version> serving 1 tool on http://127.0.0.1:8080/mcp (...)
```

- Point a Streamable HTTP MCP client at `http://127.0.0.1:8080/mcp` with `Authorization: Bearer <token from toolhost.json>` — or attach your agent: `toolhost attach <client>` (user level) / `toolhost attach --project <client>` (this folder). The server entry is always named `local-gateway`.
- Tool calls append `tool_call` records to `toolhost_audit.jsonl` in the env's state dir; rejected `gateway__call` requests and failed authentication are recorded too.

| Capability | What ships |
|---|---|
| One endpoint | An MCP proxy for upstream stdio, Streamable HTTP, and SSE servers — every client sees one `local-gateway` entry. |
| Server registry | `toolhost mcp add/list/get/remove/enable/disable/test/tools/import` — the familiar `X mcp` verbs, normalized into `toolhost.json`. |
| Access | Downstream bearer check; upstream none, bearer, client credentials, or OAuth with PKCE. |
| Agent controls | `gateway__discover/call/enable/disable/request` — five verbs, a pull-based control plane; `status`/`audit` stay CLI-side. |
| Petitions | Agents ask via `gateway__request` (tools, or whole proposed servers); humans answer with `toolhost requests --accept/--deny`. |
| Tool allowlist | `core.Resolve` registers only discovered, approved, enabled tools; invisible tools have no call handler. |
| Audit log | JSONL events for governed calls, auth failures, agent governance actions, backend errors, and reloads. |
| Local operation | One Go binary, JSON config, no database; config edits hot-reload the live surface. |

## Three-tier gate

![The three-tier gate: discovered, approved, enabled](assets/tiers.svg)

| Tier | Owner | Effect |
|---|---|---|
| Discovered | Upstream server | Appears in the catalog; cannot be called yet. |
| Approved | Human, via `toolhost approve` | Eligible for the live surface. |
| Enabled | Agent or human, within approved set | Visible and callable as `backend__tool`. |

- `discovered ≠ approved ≠ enabled`. With no `enabled` list, every approved tool is enabled; `"enabled": []` exposes none.
- `gateway__request` queues an approval ask — or a proposed server. It never approves anything.

## Config

`toolhost init` writes the bearer token and empty `backends`/`approved` lists; `mcp add` fills `backends`. Hand-editing works too — this is the whole shape:

```json
{
  "listen": "127.0.0.1:8080",
  "token": "env:TOOLHOST_TOKEN",
  "audit_log": "toolhost_audit.jsonl",
  "backends": {
    "fs": {
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    }
  },
  "approved": ["fs__read_file"]
}
```

- Config resolution, nearest scope first: `-c <path>` → this folder's private env (`~/.toolhost/projects/<id>/toolhost.json`) → `./.toolhost/toolhost.json` → `~/.toolhost/toolhost.json`. Credentials and runtime state live in the env's state dir — a project folder gains no toolhost files unless you `init --shared` one deliberately. Verify an install with `toolhost doctor`.
- For this `env:` sample, export `TOOLHOST_TOKEN` before `discover` or `serve`; `init` instead generates a usable token directly. Credential references stay literal in saved config and fail closed when unset.
- Default front door: stateless Streamable HTTP (MCP `2026-07-28`); modern clients can subscribe to list changes. `mode: "stateful"` adds held sessions for pushed `tools/list_changed` with older clients; `serve --stdio` supports spawn-only clients.
- Upstream `http`/`sse` auth, OAuth grants, federation, and per-backend timeouts are documented below.

## Reference

- [Configuration](docs/CONFIGURATION.md) — every `toolhost.json` field.
- [CLI](docs/CLI.md) — commands and flags.
- [Governance](docs/GOVERNANCE.md) — three tiers, meta-tools, requests, audit.
- [Transports](docs/TRANSPORTS.md) — front door modes, upstreams, federation.
- [Authentication](docs/AUTHENTICATION.md) — bearer, upstream auth, `env:` refs.
- [Operations](docs/OPERATIONS.md) — service install, reload, status, troubleshooting.
- [Architecture](docs/ARCHITECTURE.md) — ports, resolver, governed call.

## Deliberately absent

Tenancy, console, rate limiting, credential brokerage beyond the documented upstream auth modes. The scope is one local gateway, one bearer key, and a governed tool surface.
