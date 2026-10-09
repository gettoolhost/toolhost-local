# Operations

## Install it

```bash
brew install gettoolhost/tap/toolhost      # or: go install github.com/gettoolhost/toolhost-local/cmd/toolhost@latest
toolhost init                              # writes ~/.config/toolhost/toolhost.json (or ./toolhost.json via -c)
toolhost doctor                            # verify: config, token, port, backends, grants, service
toolhost attach <client>                   # register with your agent: claude · devin · cursor · windsurf · vscode · gemini
toolhost install                           # persist as a user service (launchd / systemd --user)
```

Default config resolution: `./toolhost.json` when present (a directory
holding a config is its own context), else the canonical home
`~/.config/toolhost/` (`$XDG_CONFIG_HOME/toolhost` when set). `-c`
overrides everything. The audit log and token store always sit beside
whichever config is in play — a relative `audit_log` anchors to the
config's directory, not the process cwd.

## Run it

```bash
toolhost serve                    # foreground, HTTP mode
toolhost serve --stdio            # as a client's spawned subprocess
toolhost install && toolhost uninstall   # user service
```

- macOS: `~/Library/LaunchAgents/ai.toolhost.gateway.plist` — RunAtLoad +
  KeepAlive; stdout/stderr → `toolhost_serve.log` beside the config.
- Linux: `~/.config/systemd/user/toolhost.service` — `Restart=always`.
- The unit runs the resolved absolute paths of the binary and config you
  invoked `install` with — build first, don't install a `go run` temp.
- Re-install is safe: bootout + bootstrap picks up the new unit.

## Observe it

```bash
toolhost status          # gateway up: live report; down: config-only view
curl localhost:8080/healthz
tail -f toolhost_audit.jsonl
```

Status shows: mode, listen, per-backend up/down + transport + tool count,
enabled/approved totals, pending agent requests, schema-drifted tools
awaiting re-approval, and (in the fallback) which oauth backends hold
stored grants.

## Backend reconnect

A backend that fails to connect at boot — or whose session dies later —
is retried automatically with exponential backoff (2s doubling to a 5m
cap, ±25% jitter). Reconnects never block the control plane: dials run
outside the live-set lock, and a config reload that lands mid-dial wins
the slot. Death signals carry the session identity, so a stale signal
can't kill a replacement session. A failed tool call is also a liveness
signal — an HTTP session can look alive after the wire dies, so a
transport-level `Call` error retires the session (caller-side timeouts
and cancellations don't). Removing the backend from config stops its
retries. Every drop and reconnect is audited (`backend_error` /
`backend_up`) and `toolhost status` shows the backend `down` meanwhile.

## Change it live

`serve` polls the config every ~1.5s — edit by hand or via CLI.

- Hot: `approved`, `enabled`, `requested`, `backends` (unchanged sessions
  kept; changed re-dialed; removed closed), `call_timeout`.
- Restart-required: `listen`, `token`, `audit_log`, `mode` — logged,
  current values kept.
- Atomic saves (tmp+rename) mean the watcher never reads a torn write.
  An invalid config keeps the current surface and audits `reload` + err.
- Modern stateless subscribers, legacy stateful clients, and stdio clients
  can receive `tools/list_changed`. The `toolhost__list`/`search`/`call`
  meta-tools provide a pull path when the client does not refresh.

## Files

| path | purpose |
|---|---|
| `toolhost.json` | config — the only state toolhost keeps |
| `toolhost_tokens.json` | upstream OAuth grants (beside config) |
| `toolhost_audit.jsonl` | append-only evidence |
| `toolhost_serve.log` | service-mode stdout/stderr (beside config) |

## Troubleshooting

| symptom | check |
|---|---|
| `env var X is unset or empty` | export it before `serve`/`status` — load fails closed |
| `backend X unreachable` | stderr line at boot/reload + `backend_error` audit; `toolhost status` shows it `down` |
| `skipped be__tool: ...` | name can't be safely namespaced — audited `backend_error` |
| oauth backend fails | `toolhost status` shows `grant MISSING` → `toolhost auth <name>` |
| `client is closing` after a denied call | go-sdk < v1.8.0 bug — upgrade; denied calls must not kill sessions |
| call hangs | impossible past `call_timeout` — check the audit for the `err` |
| service won't start | `toolhost_serve.log` (macOS) / `journalctl --user -u toolhost` (Linux) |

## Upgrade

`brew upgrade toolhost`, `go install ...@latest`, or rebuild from source
and restart the process / reinstall the service. Config format is
additive; `mode`/`call_timeout` default when absent. Releases are
`v0.0.x` patch bumps — breaking changes land only between minor series.

## Release trust chain

Tag `v0.0.x` and push — `.github/workflows/release.yml` does the rest:
GoReleaser builds the archives plus a `ghcr.io/gettoolhost/toolhost` OCI
image, pushes a Homebrew cask to `gettoolhost/homebrew-tap`, publishes
`server.json` to the MCP registry, and creates a **draft** GitHub
release. CI then signs `checksums.txt` with cosign keyless
(`checksums.txt.sigstore.json`), attests build provenance for every
release artifact, re-downloads the artifacts, verifies the checksums
signature (`cosign verify-blob`, pinned to the release workflow's
certificate identity), re-verifies every checksum against it, runs
`gh attestation verify` on each archive, and only then un-drafts.

Note what the draft gates: the ghcr image, Homebrew cask, and MCP
registry entry publish inside the GoReleaser step, before the checksum
re-verification runs. Only the GitHub release page is held back — a
failed verify leaves a draft release plus already-published external
artifacts to clean up. The signature and attestation are for consumers
to verify:

```bash
gh attestation verify toolhost_darwin_arm64.tar.gz -R gettoolhost/toolhost-local
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/gettoolhost/toolhost-local/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

OpenSSF Scorecard runs on every push to main and weekly.

The tap push needs a credential that can write both repos: set a
`TAP_GITHUB_TOKEN` Actions secret (PAT or GitHub App token with repo
scope on `toolhost-local` and `homebrew-tap`). Without it the workflow
falls back to `GITHUB_TOKEN` and the cask push fails.
