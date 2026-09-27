# Contributing

Issues and PRs welcome. The bar is small and firm:

```bash
go build ./...
go vet ./...                 # any output is a failure
gofmt -l .                   # any output is a failure
go test ./... -count=1
```

## The rules that don't bend

- `internal/core.Resolve` is the only place tool visibility/callability
  is decided. Invisible = uncallable is structural, not a check.
- `discovered != approved != enabled` — three tiers, always.
- Fail closed: bad names skip, collisions error, dead backends
  contribute zero tools.
- Every governed action leaves an audit record.
- No database, no JS, no monorepo. A new dependency needs a reason in
  the PR description.

## Agent users

`.devin/skills/toolhost-local-gateway/` teaches agents to operate the product.
If you hit friction, drop a note in its `feedback/` folder — see the
README there.
