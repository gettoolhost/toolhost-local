# Governance

The invariant: **`discovered ≠ approved ≠ enabled`**.

| tier | who writes it | meaning |
|---|---|---|
| discovered | `toolhost discover` / catalog | the backend exposes it — nothing more |
| approved | human, `toolhost approve` | policy-permitted |
| enabled | human or agent | on the live surface, callable |

- Only enabled tools appear in `tools/list` and dispatch.
- Invisible tools are **structurally uncallable** — never registered, so
  there is no check to bypass. `gateway__call` on a non-enabled name
  audits `tool_forbidden` and returns a precise reason (unknown /
  discovered-not-approved / approved-but-disabled / schema-drifted /
  backend-down / backend-parked).
- `core.Resolve` is the only place visibility is decided.

## Schema pinning

`toolhost approve` binds the approval to the contract reviewed: it dials
the owning backend, hashes each tool's schema, and stores the pin in
`approved_schemas`. On every publish, `core.Resolve` re-hashes the live
schema — a backend that silently changes a tool's contract sees the tool
**drop off the surface** (approved-but-drifted), audited as
`schema_drift`, until a human re-approves and pins the new contract.

A drifted tool is not enabled, not listable, not callable — the agent's
refusal message names the fix (`toolhost approve <name>`). Approvals
written before 0.0.3 carry no pin and keep enforcing as before; a backend
unreachable at approve time means the approval itself fails closed — you
cannot approve a contract you cannot see.

## The `gateway__*` control plane

Always registered, unaffected by reloads, reserved namespace. Five verbs —
one per transition an agent can legitimately own; pure introspection
lives in the CLI instead:

| tool | args | does |
|---|---|---|
| `gateway__discover` | `query?`, `scope?` | the catalog — search + list folded into one revelation verb; returns `{qualified, description, approved, enabled, requested, denied, drifted}`. scope: `all`(default)/`enabled`/`approved`/`discovered` |
| `gateway__call` | `name`, `arguments` | dispatch an enabled tool by name — escape hatch for clients that ignore `tools/list_changed` |
| `gateway__enable` | `names[]` | approved → live now; refuses unapproved |
| `gateway__disable` | `names[]` | live → off now; stays approved |
| `gateway__request` | `names[]?`, `servers[]?`, `reason` | petition a human — tool approvals, or whole proposed servers (`{name, transport, url\|command+args}` — topology only, never credentials) |

What used to be agent-side introspection is now outside the tool surface:
`toolhost status` reads `GET /status` (bearer-gated admin endpoint, not a
tool) and `toolhost audit` reads the log file directly.

Trust split: **agents govern `enabled`; humans govern `approved`.**
`enable` refuses unapproved names. Agent-side writes go through the same
config file the CLI edits → same persistence, same watcher pickup.

## The request loop

1. Agent hits a locked tool → `gateway__request {names, reason}` — or
   petitions a whole new server via `servers[]`.
2. The petition lands in `toolhost_requests.json` in the env's state dir
   (deduped — never in `toolhost.json`, so a committed config stays
   clean); `toolhost requests` and `toolhost status` surface it.
3. The human answers: `toolhost requests --accept <name>` (a tool
   petition becomes a schema-pinned approval; a server petition becomes
   a backend entry in config) or `--deny <name> --reason …` (the verdict
   stays on record — the agent sees `denied` in the next discover and
   cannot re-ask). `toolhost approve --requested` still drains every
   pending tool petition in one shot.
4. Agent then `gateway__enable`s it, or the human runs `toolhost enable`.

Requesting never approves. Agents cannot approve their own asks, and
cannot add servers — a server petition is a proposal, not an entry.

## Audit

Every governed action appends one JSON object to `audit_log` (`0600`).
Event: `{ts, kind, backend, tool, ms, err, remote}`.

| kind | emitted on |
|---|---|
| `tool_call` | every dispatched call, with `ms`; `err` set on failure |
| `auth_failed` | bad/missing bearer on `/mcp`, with `remote` |
| `tool_forbidden` | call to a non-enabled name |
| `govern` | agent-initiated enable/disable/request |
| `backend_error` | unreachable backend, skipped unsafe names, lost sessions |
| `backend_up` | backend (re)connected — boot and reconnects |
| `schema_drift` | an approved tool's live schema no longer matches its pin |
| `reload` | config hot-swap; `err` set when a bad save was rejected |

Humans query it with `toolhost audit` or any JSONL tool — e.g.
`jq -c 'select(.kind=="tool_forbidden")'`; the log never contains
arguments or secrets.

The log rotates at `audit_max_mb` (default 10): the live file swaps to
`audit_log.1` and a fresh segment opens — at most ~2× the cap on disk,
always `0600`, and no write is ever dropped inside the retained window.
