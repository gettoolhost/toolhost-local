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
go build -o toolhost ./cmd/toolhost
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

Point any MCP client at `http://127.0.0.1:8080/mcp` with the bearer token.
Tools are namespaced `backend__tool`. Approving changes the config file;
`serve` picks it up on restart.

## What this is not (yet)

No tenancy, no console, no database, no upstream credential brokerage
beyond the auth block above. A single key, a single endpoint, a governed
call. The mature enterprise tree this grew from lives in `reference/` —
read-only, its own git history intact.

## Layout

`docs/ARCHITECTURE.md` is the one-page map: ports & adapters, the resolver
that owns visibility, and what lands where.
