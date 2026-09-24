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
cat toolhost-audit.jsonl        # every call, identified and timed
```

Config:

```json
{
  "listen": "127.0.0.1:8080",
  "token": "th_…",
  "audit_log": "toolhost-audit.jsonl",
  "backends": {
    "fs": { "transport": "stdio", "command": "npx",
            "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"] },
    "remote": { "transport": "http", "url": "https://mcp.example.com/mcp",
                "headers": { "Authorization": "Bearer …" } }
  },
  "approved": ["fs__read_file"]
}
```

Point any MCP client at `http://127.0.0.1:8080/mcp` with the bearer token.
Tools are namespaced `backend__tool`. Approving changes the config file;
`serve` picks it up on restart.

## What this is not (yet)

No OAuth, no tenancy, no console, no database, no upstream credential
brokerage. A single key, a single endpoint, a governed call. The mature
enterprise tree this grew from lives in `reference/` — read-only, its own
git history intact.

## Layout

`docs/ARCHITECTURE.md` is the one-page map: ports & adapters, the resolver
that owns visibility, and what lands where.
