# Authentication

Two directions, kept apart:

- **Downstream** — agents → toolhost: one bearer token.
- **Upstream** — toolhost → backends: per-backend `auth` block.

## Downstream: the bearer token

- `Authorization: Bearer <token>` on every `/mcp` request; constant-time
  compared; denials audited as `auth_failed` with the remote address.
- Minted by `toolhost init` (`th_` + 24 random bytes, hex).
- `serve --stdio` has no bearer — the spawning process owns the pipe.
- Store it as an `env:` ref (`"token": "env:TOOLHOST_TOKEN"`) if the
  config should be committable.

## Upstream: the `auth` block

| `type` | fields | behavior |
|---|---|---|
| absent / `none` | — | no upstream auth |
| `bearer` | `token` | static `Authorization: Bearer` — the API-key shape (a raw `headers` entry works too) |
| `client_credentials` | `client_id`, `client_secret`, `scopes?` | machine OAuth; token endpoint discovered via the server's Protected Resource Metadata; minted on connect |
| `oauth` | `client_id?`, `client_secret?`, `scopes?`, `redirect_url?` | auth-code + PKCE; no `client_id` means dynamic client registration |

- `auth` is invalid on `stdio` backends — subprocess creds go in `env`.
- `oauth` grant: `toolhost auth <backend>` opens the browser, listens on
  `http://127.0.0.1:8484/callback` (override with `redirect_url`; must
  match the registered/DCR'd URI). `serve`/`discover` reuse and refresh
  the stored grant — they never open a browser; a missing grant fails
  with the remedy.
- `toolhost logout <backend>` drops the stored grant.

## stdio backend environment

A spawned backend sees a **baseline + named variables**, never the whole
host environment — least privilege applies to processes, not just tools.

- Baseline: `PATH`, `HOME`, `LANG`, `TMPDIR`, runtime/platform vars —
  enough for `npx`/`uvx`/`python` to work.
- `env`: literal values or `"env:NAME"` refs — always reach the backend,
  win on collision.
- `env_allowlist`: host vars forwarded by name — the way to pass an
  already-exported credential (e.g. `["OPENAI_API_KEY"]`).
- `env_inherit: true`: the opt-out — the gateway's entire environment
  reaches the backend. Use deliberately.

## Token store

`toolhost_tokens.json` beside the config (`0600`). Holds per-backend
grants: access/refresh tokens, expiry, scopes, plus the client
registration (id/secret/token URL) needed to rebuild a refreshing token
source without re-authorizing. Never commit it — it's credentials.

## `env:` references

`token`, `env` values, `headers` values, `auth.token`,
`auth.client_id`, `auth.client_secret` accept `"env:NAME"`. Resolved at
use on copies — the on-disk file keeps the reference, so `Save` can
never persist a secret. Unset/empty vars fail closed at load.

## Files and permissions

| file | mode | is |
|---|---|---|
| `toolhost.json` | `0600` | config — committable when secrets are `env:` refs |
| `toolhost_tokens.json` | `0600` | upstream grants — never commit |
| `toolhost_audit.jsonl` | `0600` | evidence — calls, denials, reloads |

All three are only as safe as your user account — that's the boundary
for a local tool. See `SECURITY.md` for the threat model.
