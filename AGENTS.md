# AGENTS.md

Toolhost OSS kernel — one governed MCP front door. Pure Go module
(`github.com/gettoolhost/toolhost-local`, go 1.26.2), no database, no JS,
no monorepo.

## Build, test, run

```bash
go build ./...                    # build
go vet ./... && gofmt -l .        # static checks — any output is a failure
go test ./... -count=1            # tests; internal/app has the e2e wire test
go build -o toolhost ./cmd/toolhost && ./toolhost init
```

## Structure

Ports & adapters — see `docs/ARCHITECTURE.md`. Dependency direction is one
way: adapters → `internal/core`. `core.Resolve` is the ONLY place tool
visibility/callability is decided; keep it that way (invisible = uncallable
is a structural property, not a check). Namespacing is `backend__tool` —
the grammar and its constraints live in `internal/namespace`; do not change
the separator without understanding why `::` was rejected.

## Invariants worth keeping

- `discovered != approved != enabled` — `core.Resolve` exposes only
  discovered, approved, enabled tools; absent `enabled` means all approved.
- Gateway calls and agent governance actions leave JSONL audit records
  (`internal/audit`).
- Ambiguity denies: bad names skip, collisions are hard errors, dead
  backends contribute zero tools.
- stdio backends see a baseline env + explicitly named vars only (`env`,
  `env_allowlist`); `env_inherit` is the opt-out — keep it that way.
- Fail closed; never default-open on a missing or malformed anything.

## Local files

- Keep private notes, drafts, generated source art, credentials, and runtime
  logs in ignored `.local/` or the existing ignored runtime paths.
- Tracked docs and assets are for users; review each before commit.
