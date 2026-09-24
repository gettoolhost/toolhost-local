# CLI

`toolhost <verb> [-c config]`. Default config: `toolhost.json` in cwd.

## Lifecycle

```bash
toolhost init          # write config + fresh th_* bearer token; refuses overwrite
toolhost serve         # HTTP /mcp on <listen>; bearer-gated; watches config
toolhost serve --stdio # same surface over stdin/stdout — no bearer, pipe owner is authority
toolhost status        # live report via toolhost__status; config-only fallback if down
toolhost install       # launchd (macOS) / systemd --user (Linux) service
toolhost uninstall     # stop + remove the service
toolhost version
```

## Governance

```bash
toolhost discover                      # full catalog: ✓ enabled, ○ approved-disabled, blank discovered
toolhost approve fs__read_file ...     # add to approved; consumes matching requested entries
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

## Upstream auth

```bash
toolhost auth <backend>    # oauth: browser + loopback callback; cc: token exchange
toolhost logout <backend>  # drop the stored grant — backend fails closed until re-auth
```

- Grants persist to `toolhost_tokens.json` beside the config (`0600`).
- `serve`/`discover` reuse + refresh grants; they never open a browser —
  a missing grant fails with `run: toolhost auth <backend>`.

## Exit behavior

- Unknown command / bad usage: exit 2 with usage.
- Load/validate/connect failures: exit 1 with `toolhost: <err>`.
- `init` never overwrites an existing config — delete first if you mean it.
