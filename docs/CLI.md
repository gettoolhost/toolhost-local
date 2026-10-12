# CLI

`toolhost <verb> [-c config]`. Config resolution, nearest scope first:
`-c` → this folder's private env (`~/.toolhost/projects/<id>/toolhost.json`)
→ `./.toolhost/toolhost.json` → `~/.toolhost/toolhost.json` (`$TOOLHOST_HOME`
relocates the whole home for testing).

Everything sensitive lives in `~/.toolhost/` — credentials, OAuth grants,
audit logs, and the agent request queue. A project folder never gains a
toolhost file unless you `init --shared` one on purpose.

## Environments

```bash
toolhost init            # global env: ~/.toolhost/toolhost.json + fresh th_* token
toolhost init --project  # private env for THIS folder: ~/.toolhost/projects/<id>/
toolhost init --shared   # committable ./.toolhost/toolhost.json — env: ref, no literal secret
```

- `init` never overwrites — delete first if you mean it.
- `--project` is private-by-default: the folder stays clean; nothing to
  commit, nothing to leak. `--shared` is the opt-in for teams — it writes
  `"token": "env:TOOLHOST_TOKEN"`, safe for source control (stdio serving
  needs no token; HTTP serving does).
- A private env shadows a committed `./.toolhost/toolhost.json` in the same folder —
  your scope wins, like `local > project > user` in the clients.

## Lifecycle

```bash
toolhost doctor        # verify an install: config, token, port, backends, grants, service
toolhost attach            # survey every known client: attached / not attached / not set up
toolhost attach <client>   # write the user-level local-gateway entry — claude · claude-desktop · cline · codex · cursor · devin · gemini · opencode · roo · vscode · windsurf · zed
toolhost attach --project <client>  # write THIS folder's project file — claude · codex · cursor · devin · gemini · opencode · roo · vscode · zed
toolhost attach --print    # print the mcpServers JSON for any other client
toolhost attach --stdio <client>  # attach the spawn form (serve --stdio, no bearer)
toolhost attach --dry-run <client>  # show the planned merge (create/update/attached) without writing
toolhost serve         # HTTP /mcp on <listen>; bearer-gated; watches config
toolhost serve --stdio # same surface over stdin/stdout — no bearer, pipe owner is authority
toolhost status        # live report via toolhost__status; config-only fallback if down
toolhost install       # launchd (macOS) / systemd --user (Linux) service
toolhost uninstall     # stop + remove the service
toolhost version
```

- The MCP server name is always `local-gateway` — same name at user and
  project scope, so the client's own nearest-scope precedence makes the
  project env win where both exist.
- `--project` writes a tokenless stdio spawn (`toolhost serve --stdio`
  on PATH — portable, committable) and needs no config or bearer: the
  spawned process resolves the folder's environment itself. Clients may
  hold project servers pending trust — open the folder and approve when
  prompted.
- File-backed clients are merged ownership-aware: an identical
  `local-gateway` entry is a no-op, a stale one is refreshed in place
  preserving keys the client added, and an entry under our name
  that isn't this gateway's is refused rather than overwritten. Writes
  are compare-and-swap — if the file changed between read and write,
  attach fails instead of losing it. `devin` and `codex` attach through
  their own CLIs (`codex` reads the bearer from `$TOOLHOST_TOKEN`, never
  the literal — export it where codex runs).
- `claude-desktop` only speaks stdio — it gets the spawn form regardless
  of `--stdio`. `zed`'s entry shape is `context_servers` with a bare
  command string; `opencode`'s is `mcp` with `"type": "local"|"remote"`.
- Conditional clients are only written into a config that already
  exists — attach never creates a bare config for a client that isn't
  set up.
- Clients with a documented user-level skill directory (claude, devin)
  also get a managed `SKILL.md` teaching the `toolhost__*` control
  plane — ours to refresh on re-attach, never touching a file without
  the toolhost marker.

## Governance

```bash
toolhost discover                      # full catalog: ✓ enabled, ○ approved-disabled, blank discovered
toolhost approve fs__read_file ...     # add to approved; pins each tool's schema — drift lapses the approval
toolhost approve --requested           # approve every pending agent request (drains the queue)
toolhost revoke <names...>             # remove from approved (and thus the surface)
toolhost enable <names...>             # allowlist-mode: put approved tools on the live surface
toolhost enable                        # clear the allowlist — all approved enabled
toolhost enable --only <names...>      # live surface becomes exactly these
toolhost disable <names...>            # hide approved tools without un-approving
```

- Approve/revoke/edit write the config atomically; a running `serve` picks
  it up in ~1.5s.
- `enable` while no `enabled` list exists is a no-op (all approved already
  on). `disable` materializes the list as `approved ∖ names`.
- Enabling an unapproved tool fails with the remedy:
  `run: toolhost approve <name>`.

## Evidence

```bash
toolhost audit                 # last 20 events, newest last
toolhost audit -n 100          # more history (bounded tail, max 500)
toolhost audit -kind govern    # only agent governance actions
```

- Reads `audit_log` directly — the gateway need not be running, and the
  rotated `.1` generation backfills whatever the live segment doesn't
  cover. The log never carries arguments or secrets.

## Upstream auth

```bash
toolhost auth <backend>    # oauth: browser + loopback callback; cc: token exchange
toolhost logout <backend>  # drop the stored grant — backend fails closed until re-auth
```

- Grants persist to `toolhost_tokens.json` in the env's state dir (`0600`)
  — beside the config for home envs, keyed under `~/.toolhost/projects/`
  for folder envs.
- `serve`/`discover` reuse + refresh grants; they never open a browser —
  a missing grant fails with `run: toolhost auth <backend>`.

## Exit behavior

- Unknown command / bad usage: exit 2 with usage.
- Load/validate/connect failures: exit 1 with `toolhost: <err>`.
- `init` never overwrites an existing config — delete first if you mean it.
