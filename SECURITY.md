# Security

toolhost-local holds credentials: a bearer token for its own `/mcp`
endpoint and upstream grants in `toolhost_tokens.json`. Take reports
seriously — we do.

## Report

Open a private security advisory on GitHub
(`Security → Advisories → Report a vulnerability`) or email the
maintainer listed on the repo. Do not open public issues for
vulnerabilities.

## Threat model

- Local-first: the default bind is `127.0.0.1`. Binding a non-loopback
  address exposes the gateway to your network — the bearer token is the
  only gate; treat it accordingly.
- `toolhost.json`, `toolhost_tokens.json`, and the audit log are written
  `0600`. They are only as safe as your user account — that is the
  boundary for a local tool.
- Secrets in config support `env:` references — prefer them so the file
  can be committed safely. Never commit `toolhost_tokens.json`.
- Every governed action lands in the JSONL audit log; denials are
  recorded too (`auth_failed`, `tool_forbidden`).
