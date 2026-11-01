# CLI

`toolhost <verb> [-c config]`. The human's whole surface — servers,
policy, surface, runtime, evidence. Agents never see this; their world
is the `local-gateway` MCP server and its `gateway__*` control plane.

Every verb works with zero flags; flags only refine. `-c` picks the env;
else nearest scope wins: this folder's private env
(`~/.toolhost/projects/<id>/toolhost.json`) → `./.toolhost/toolhost.json`
→ `~/.toolhost/toolhost.json` (`$TOOLHOST_HOME` relocates the home).

Everything sensitive lives in `~/.toolhost/` — credentials, OAuth grants,
audit logs, and the agent request queue. A project folder never gains a
toolhost file unless `init --shared` made one on purpose.

## The path

```bash
toolhost                  # no config → this path; config exists → status
toolhost init             # ~/.toolhost/toolhost.json + fresh th_* token
toolhost mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp
toolhost discover         # every tool the backends expose
toolhost approve fs__read_file
toolhost serve            # bearer-gated /mcp
toolhost attach claude    # give the agent the one server: local-gateway
```

Six commands, no flags — the default of every verb is the common intent.

## Environments

```bash
toolhost init            # global env
toolhost init --project  # private env for THIS folder — folder stays clean
toolhost init --shared   # committable ./.toolhost/toolhost.json (env: ref token)
```

- `init` never overwrites — delete first if you mean it.
- A private env shadows a committed `./.toolhost/toolhost.json` in the
  same folder — `local > project > user`, like the clients.

## Servers — the registry

`toolhost mcp` is the federated `X mcp`: the grammar every client CLI
already taught, pointed at `toolhost.json` instead of the client's config.

```bash
toolhost mcp add <name> <command…>          # stdio — the tail is literal
toolhost mcp add <name> <https://url>       # http by inference
toolhost mcp add                            # bare on a TTY → asks in words
```

Flags come before the name; everything after it is the command as-is.
A lone `--` is accepted and stripped (muscle memory from `claude mcp add`).

```bash
toolhost mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp
toolhost mcp add github https://api.githubcopilot.com/mcp/ --token env:GH_TOKEN
toolhost mcp add linear --url https://mcp.linear.app/mcp --auth oauth
toolhost mcp add corp --url https://mcp.corp/mcp --auth cc \
    --client-id toolhost --client-secret env:CORP_SECRET --scopes tools
toolhost mcp add db -e PG_DSN=env:PG_DSN --env-allowlist PATH \
    -- npx -y @postgres/mcp-server
toolhost mcp add github https://… --token env:GH_TOKEN --approve-all
toolhost mcp add edge --url http://127.0.0.1:8188/mcp --passthrough
toolhost mcp add fs --json '{"command":"npx","args":["-y","…","/tmp"]}'
toolhost mcp add github --from claude       # pull one server out of a client config
```

| flag | effect |
|---|---|
| `-t stdio\|http` | force transport (else inferred) |
| `-e KEY=val` · `-e KEY=env:VAR` | stdio env — `env:` is the rule, never literals; a literal credential warns |
| `--env-allowlist a,b` | named host vars forwarded to the spawn |
| `--env-inherit` | pass the whole host env — the explicit opt-out |
| `--cwd dir` | spawn working directory |
| `-H "K: v"` | http headers (repeatable) |
| `--token env:VAR` | bearer auth — the variable NAME, never the literal |
| `--auth oauth` | deferred grant — finish with `toolhost mcp auth <name>` |
| `--auth cc --client-id … --client-secret env:VAR --scopes a,b` | client credentials |
| `--passthrough` | the backend is itself a gateway |
| `--approve-all` | trust the server — every tool becomes eligible |
| `--approve a,b` | pin specific tools at add time (pins land on first connect) |
| `--disabled` | registered but not connected |
| `--json '<entry>'` · `--json -` | paste a client-style entry (stdin with `-`) |
| `--from <client>` | extract one named server from a client's config |
| `-s user\|project\|shared` | which env file to write |
| `--dry-run` | print the `backends{}` diff, write nothing |

Name rules: the name becomes the `backend__tool` namespace — lowercase,
no `__`, not the reserved `gateway`/`toolhost`. Duplicates refuse —
`mcp remove` first. `add` never dials; a missing stdio command warns but
writes anyway (that is `mcp test`'s job).

```bash
toolhost mcp list                  # name · transport · state · tools · counts
toolhost mcp get <name>            # one backend: entry, grant, tools, error
toolhost mcp remove <name>         # refuses while its tools hold approvals
        --prune-approvals          # drop the tool approvals with it
        --dry-run
toolhost mcp disable <name>        # park: config kept, never dialed, tools off the surface
toolhost mcp enable <name>         # reconnect a parked backend
toolhost mcp test <name>           # dial → handshake → tool count → auth state
toolhost mcp tools <name>          # its tools with input schemas
toolhost mcp auth <name>           # OAuth browser flow / client-credentials exchange
toolhost mcp logout <name>         # drop the stored grant — fails closed until re-auth
toolhost mcp import <client>       # absorb every server in a client's config
        --dry-run                  # show the moves first
```

## Policy — the human-only gate

```bash
toolhost discover                      # catalog: ✓ enabled · ○ approved · ! drifted · blank discovered
toolhost approve fs__read_file …       # approve + pin each tool's schema — drift lapses it
toolhost approve --requested           # approve every pending agent request
toolhost approve --dry-run             # preview the pins/diff
toolhost revoke <names…>               # drop approval — tools leave the surface
toolhost requests                      # pending agent petitions — tools AND proposed servers
toolhost requests --accept <name>      # grant: approve the tool / materialize the server
toolhost requests --deny <name>        # close it — visible to the agent as denied
```

- approve/revoke/enable/disable write the config atomically; a running
  `serve` picks it up in ~1.5s.
- No agent-facing verb can approve anything. Petitions file requests;
  only these commands answer them.

## Surface

```bash
toolhost enable              # no args = every approved tool live (clears the allowlist)
toolhost enable a b          # add these to the surface
toolhost enable --only a b   # the surface becomes exactly these
toolhost disable a b         # hide approved tools — they stay approved
```

## Run & wire

```bash
toolhost serve                 # /mcp on <listen>, bearer-gated, watches config
toolhost serve --stdio         # same surface over a pipe — spawn owner is authority
toolhost attach                # survey: attached / set-up / absent, every client
toolhost attach <client>       # write the local-gateway entry (user scope)
toolhost attach --project <client>   # this folder's file — tokenless stdio spawn
        --stdio · --print · --dry-run
toolhost install | uninstall   # launchd (macOS) / systemd --user service
```

- The MCP server entry name is `local-gateway` at every scope — a
  project entry shadows a user one. Entries under other names are never
  ours to touch.
- File-backed clients merge ownership-aware (compare-and-swap); `devin`
  and `codex` attach through their own `mcp add` CLIs.
- `attach` adds our entry only — `mcp import <client>` is the move that
  absorbs a client's existing servers into `toolhost.json`.

## Evidence

```bash
toolhost status         # gateway up/down, backends, counts, pending requests
toolhost status --json  # the machine form — what a harness polls
toolhost audit          # last 20 events, newest last
        -n 100 · -kind govern
```

The log is JSONL in the env's state dir — tool calls, refusals,
governance actions, reloads. Never arguments, never secrets.

## Universal

```bash
-c <path>     # env override — else nearest scope wins
--json        # machine output on read verbs (status, discover, requests,
              # mcp list|get|tools|test)
--dry-run     # on writes — preview the diff (approve, revoke,
              # mcp add|remove|import)
```

```bash
toolhost help              # verbs grouped by job
toolhost help <verb>       # one verb, with examples
toolhost completion bash|zsh|fish
toolhost version
```

## Exit codes

`0` ok · `1` runtime failure (includes refusals: cascades, duplicates,
unapproved enables) · `2` usage error.

## The agent's surface, for contrast

Agents attach to `local-gateway` and see the governed tools plus five
verbs — `gateway__discover`, `gateway__enable`, `gateway__disable`,
`gateway__call`, `gateway__request` (tools and server proposals). No
verb on the MCP surface can add a server or grant an approval; the
envelope is owned here, by this CLI, in `toolhost.json`.
