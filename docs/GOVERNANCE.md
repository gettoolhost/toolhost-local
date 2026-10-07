# Governance

The invariant: **`discovered ≠ approved ≠ enabled`**.

| tier | who writes it | meaning |
|---|---|---|
| discovered | `toolhost discover` / catalog | the backend exposes it — nothing more |
| approved | human, `toolhost approve` | policy-permitted |
| enabled | human or agent | on the live surface, callable |

- Only enabled tools appear in `tools/list` and dispatch.
- Invisible tools are **structurally uncallable** — never registered, so
  there is no check to bypass. `toolhost__call` on a non-enabled name
  audits `tool_forbidden` and returns a precise reason (unknown /
  discovered-not-approved / approved-but-disabled).
- `core.Resolve` is the only place visibility is decided.

## The `toolhost__*` control plane

Always registered, unaffected by reloads, reserved namespace:

| tool | args | does |
|---|---|---|
| `toolhost__search` | `query` | catalog search by name/description; returns `{qualified, description, approved, enabled, requested}` |
| `toolhost__list` | `scope`: `enabled`(default)/`approved`/`all` | the catalog filtered |
| `toolhost__call` | `name`, `arguments` | dispatch an enabled tool by name — escape hatch for clients that ignore `tools/list_changed` |
| `toolhost__enable` | `names[]` | approved → live now; refuses unapproved |
| `toolhost__disable` | `names[]` | live → off now; stays approved |
| `toolhost__request` | `names[]`, `reason` | queue an approval ask in `requested` |
| `toolhost__audit` | `limit?`, `kind?` | tail the audit log (newest last) — introspection without file access |
| `toolhost__status` | — | mode, per-backend up/tools, approved/enabled counts, pending requests |

Trust split: **agents govern `enabled`; humans govern `approved`.**
`enable` refuses unapproved names. Agent-side writes go through the same
config file the CLI edits → same persistence, same watcher pickup.

## The request loop

1. Agent hits a locked tool → `toolhost__request {names, reason}`.
2. Request lands in `requested` (deduped); `toolhost status` /
   `toolhost__status` surface it.
3. Human runs `toolhost approve <name>` — the queue entry is consumed.
4. Agent then `toolhost__enable` or the human runs `toolhost enable`.

Requesting never approves. Agents cannot approve their own asks.

## Audit

Every governed action appends one JSON object to `audit_log` (`0600`).
Event: `{ts, kind, backend, tool, ms, err, remote}`.

| kind | emitted on |
|---|---|
| `tool_call` | every dispatched call, with `ms`; `err` set on failure |
| `auth_failed` | bad/missing bearer on `/mcp`, with `remote` |
| `tool_forbidden` | call to a non-enabled name |
| `govern` | agent-initiated enable/disable/request |
| `backend_error` | unreachable backend, skipped unsafe names |
| `reload` | config hot-swap; `err` set when a bad save was rejected |

Humans query the file with any JSONL tool — e.g.
`jq -c 'select(.kind=="tool_forbidden")'`. Agents read it through
`toolhost__audit` (bounded tail, optional `kind` filter); the log never
contains arguments or secrets, so exposing it is safe.
