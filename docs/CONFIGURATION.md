# Configuration

One file: `toolhost.json` (default; `-c <path>` on every command).
Written `0600`, saved atomically (tmp+rename). Full example:
`toolhost.example.json`.

## Top level

| field | type | default | notes |
|---|---|---|---|
| `listen` | string | `127.0.0.1:8080` | bind for `/mcp` + `/healthz`. Non-loopback exposes the LAN — bearer is the only gate |
| `token` | string | — | gateway bearer token; `env:` ref allowed |
| `audit_log` | string | `toolhost_audit.jsonl` | JSONL evidence file, `0600` |
| `mode` | `"stateless"` \| `"stateful"` | `stateless` | restart-required |
| `call_timeout` | Go duration | `60s` | bounds every upstream call; caller's tighter deadline wins |
| `backends` | object | `{}` | name → backend block; name becomes the `backend__` prefix |
| `approved` | string[] | `[]` | qualified names a human permits |
| `enabled` | string[] | absent | live-surface allowlist — see semantics below |
| `requested` | object[] | — | agent-filed requests; written by `toolhost__request`, consumed by `approve` |

`enabled` semantics: **absent = all approved tools enabled**; present
(including `[]`) = only `enabled ∩ approved` is live. Never write it by
hand unless you mean it — `toolhost enable/disable` manages it.

`requested` entries: `{ "name", "reason", "at" }` — deduped by name.

## Backend

| field | applies to | notes |
|---|---|---|
| `transport` | all | `stdio` \| `http` \| `sse` (`streamable_http` alias for `http`) |
| `command`, `args`, `env` | stdio | subprocess argv + environment — `env` values may be `env:` refs |
| `env_allowlist` | stdio | host env vars forwarded on top of the baseline (`PATH`, `HOME`, runtime vars) |
| `env_inherit` | stdio | forward the *whole* host environment — the least-safe option; mutually exclusive with `env_allowlist` |
| `url` | http, sse | MCP endpoint |
| `headers` | http, sse | static headers; values may be `env:` refs |
| `auth` | http, sse | upstream auth — see AUTHENTICATION.md. Invalid on stdio |
| `passthrough` | http, sse | upstream is a federated gateway — trust qualified names, drop its `toolhost__*` |
| `call_timeout` | all | per-backend override of the global |

Backend names: `^[a-zA-Z0-9_-]+$` and not `toolhost` (reserved for the
meta-plane). Names are the namespace — rename = re-approval.

## `env:` references

Credential fields accept `"env:NAME"` instead of a literal:

- top-level `token`
- backend `env` values
- backend `headers` values
- `auth.token`, `auth.client_id`, `auth.client_secret`

Rules: resolved at use (connect/serve/status/auth), never written back —
`Save` persists the reference. Unset or empty var fails closed at load,
not at first call.

## Validation

`config.Load` fails closed on: bad JSON, unknown `mode`, missing token
resolution, bad `call_timeout`, reserved/invalid backend names, missing
`command`/`url`, `auth` on stdio, `env`/`env_allowlist`/`env_inherit` on
non-stdio, `env_inherit` + `env_allowlist` together, malformed qualified
names in `approved`/`enabled`.

## Hot reload

`serve` polls the file every ~1.5s. Live-swapped: `approved`, `enabled`,
`requested`, `backends` (unchanged sessions kept; changed re-dialed),
`call_timeout`. Restart-required: `listen`, `token`, `audit_log`, `mode`
— a change is logged and current values kept. Invalid saves keep the
current surface and audit a `reload` event with the error.
