// help.go — the CLI's self-explanation: grouped verb table, per-verb
// help, near-miss suggestions, and shell completion. One table is the
// single source help and completion both render from.
package app

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Verb is one line of the command surface — name, job group, one-line
// blurb, and the deeper help shown by `toolhost help <verb>`.
type Verb struct {
	Name, Group, Blurb, Long string
}

// Verbs is the command surface, grouped by job in flow order.
var Verbs = []Verb{
	{"init", "setup", "write a config with a fresh bearer token",
		"toolhost init [--project|--shared]\n\n" +
			"  default:    ~/.toolhost/toolhost.json + fresh th_* token (0600)\n" +
			"  --project   this folder's private env under ~/.toolhost/projects/\n" +
			"  --shared    committable ./.toolhost/toolhost.json — env: token ref, no secret\n\n" +
			"init never overwrites — delete the file first if you mean it."},
	{"doctor", "setup", "check an install end to end — config, token, backends, grants",
		"toolhost doctor\n\n" +
			"Verifies: config parses, token resolves, port free, each backend dials,\n" +
			"oauth grants present, service state. Per-check remedies on failure."},
	{"mcp", "servers", "the server registry — add, list, get, remove, enable, disable, test, tools, auth, logout, import",
		"toolhost mcp <sub>\n\n" +
			"  add <name> <command…|url>   register a backend (flags anywhere; --json / --from / -s scope)\n" +
			"  list                        the registry — name, transport, state, approved count\n" +
			"  get <name>                  one backend's entry, grant, approved tools\n" +
			"  remove <name>               delete it — refuses while it holds approvals (--prune-approvals)\n" +
			"  enable|disable <name>       reconnect / park a backend\n" +
			"  test <name>                 dial it — handshake, tool count, auth state\n" +
			"  tools <name>                its tools with schemas (--names-only)\n" +
			"  auth|logout <name>          upstream OAuth grant / drop the grant\n" +
			"  import <client>             absorb every server in a client's config (--dry-run)\n\n" +
			"Examples:\n" +
			"  toolhost mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp\n" +
			"  toolhost mcp add github https://api.githubcopilot.com/mcp/ --token env:GH_TOKEN\n" +
			"  toolhost mcp import claude"},
	{"discover", "policy", "every tool the backends expose — the governed catalog",
		"toolhost discover [--json]\n\n" +
			"Dials every backend and prints each tool marked:\n" +
			"  ✓ enabled · ○ approved-but-disabled · ! schema drifted · blank discovered only"},
	{"approve", "policy", "approve tool names (schema-pinned) or --requested petitions",
		"toolhost approve <backend__tool…> [--requested] [--dry-run]\n\n" +
			"Approving dials the owning backend and pins each tool's schema hash — the\n" +
			"grant binds to the contract reviewed. Upstream schema drift lapses the\n" +
			"approval until re-approved.\n\n" +
			"  --requested   approve every pending agent tool petition\n" +
			"  --dry-run     show the pins and diff without writing"},
	{"revoke", "policy", "drop approvals — tools leave the surface",
		"toolhost revoke <backend__tool…> [--dry-run]\n\n" +
			"Revoking removes the approval AND its schema pin — the tool leaves the\n" +
			"surface and is uncallable within ~2s of a running serve."},
	{"requests", "policy", "review agent petitions — --accept / --deny",
		"toolhost requests [--all] [--accept <name>] [--deny <name> --reason …]\n\n" +
			"The queue gateway__request files into: tool petitions and proposed\n" +
			"servers. Accepting a tool approves it; accepting a server materializes\n" +
			"its spec into config as a backend. Denied petitions stay visible to the\n" +
			"agent as refused.\n\n" +
			"  --all     include denied petitions in the listing\n" +
			"  --json    machine-readable queue"},
	{"enable", "surface", "put approved tools on the live surface (no args = all)",
		"toolhost enable [names…] [--only names…]\n\n" +
			"  no args     every approved tool is live (clears the allowlist)\n" +
			"  names       add these to the surface\n" +
			"  --only      the surface becomes exactly these\n\n" +
			"Enabling an unapproved tool refuses — approve first."},
	{"disable", "surface", "hide approved tools without un-approving them",
		"toolhost disable <names…>\n\n" +
			"Hidden tools leave the surface and are uncallable but keep their\n" +
			"approval — enable brings them back."},
	{"serve", "run", "serve the governed /mcp (or --stdio)",
		"toolhost serve [--stdio]\n\n" +
			"  default:    http://<listen>/mcp, Authorization: Bearer <token>\n" +
			"  --stdio:    MCP over stdin/stdout — the spawning process is authority,\n" +
			"              no bearer needed\n\n" +
			"The gateway watches the config file — policy and backend edits go live\n" +
			"in ~1.5s, emitting tools/list_changed to subscribed clients.\n" +
			"Admin reads: GET /status (bearer-gated), GET /healthz (open)."},
	{"attach", "run", "write the local-gateway entry into a client",
		"toolhost attach [<client>] [--project|--stdio|--print|--dry-run]\n\n" +
			"  no client   survey every known client — attached, set-up, absent\n" +
			"  <client>    merge the local-gateway entry into its config (user scope)\n" +
			"  --project   this folder's file — tokenless stdio spawn\n" +
			"  --stdio     spawn the gateway instead of HTTP\n" +
			"  --print     show the entry JSON without writing\n" +
			"  --dry-run   show the planned merge\n\n" +
			"Clients: claude, claude-desktop, cline, codex, cursor, devin, gemini,\n" +
			"opencode, roo, vscode, windsurf, zed."},
	{"install", "run", "register serve as a service (launchd / systemd --user)",
		"toolhost install\n\nRegisters `toolhost serve` as a user service for the current env."},
	{"uninstall", "run", "remove the service", "toolhost uninstall — stop and remove the service."},
	{"status", "evidence", "gateway health — backends, counts, petitions (--json)",
		"toolhost status [--json]\n\n" +
			"Gateway up/down, per-backend liveness, tool counts, pending petitions.\n" +
			"Live data comes from GET /status; a down gateway falls back to the\n" +
			"config view. --json is the harness's polling surface."},
	{"audit", "evidence", "tail the evidence log (-n, -kind)",
		"toolhost audit [-n 20] [-kind tool_call]\n\n" +
			"JSONL in the env's state dir: calls, refusals, governance actions,\n" +
			"reloads, backend errors. Never arguments, never secrets."},
	{"version", "", "print the version", "toolhost version"},
	{"completion", "", "shell completion script — bash|zsh|fish",
		"toolhost completion <bash|zsh|fish>\n\nPrints the script — no dependencies, source it or save it."},
	{"help", "", "this list, or one verb: toolhost help <verb>", "toolhost help [<verb>]"},
}

// Help renders the grouped verb list, or one verb's deeper help.
func Help(argv []string, w io.Writer) {
	if len(argv) > 0 {
		for _, v := range Verbs {
			if v.Name == argv[0] {
				fmt.Fprintf(w, "%s\n\n%s\n", v.Blurb, v.Long)
				return
			}
		}
		fmt.Fprintf(w, "no help for %q\n\n", argv[0])
	}
	fmt.Fprint(w, `toolhost — one governed front door between your agent and your MCP servers

`)
	last := ""
	for _, v := range Verbs {
		if v.Group != last {
			if last != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "  %s\n", v.Group)
			last = v.Group
		}
		fmt.Fprintf(w, "    %-10s %s\n", v.Name, v.Blurb)
	}
	fmt.Fprint(w, `
all commands take -c <path> — nearest scope wins:
this folder's private env → ./.toolhost/toolhost.json → ~/.toolhost/toolhost.json
(TOOLHOST_HOME honored). every write honors --dry-run; reads honor --json.

toolhost help <verb> for one verb's full form.
`)
}

// Suggest offers the nearest verb on a typo — Levenshtein ≤ 2 wins.
func Suggest(got string) string {
	got = strings.ToLower(got)
	best, dist := "", 3
	for _, v := range Verbs {
		if d := levenshtein(got, strings.ToLower(v.Name)); d < dist {
			best, dist = v.Name, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur := make([]int, len(br)+1)
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev = cur
	}
	return prev[len(br)]
}

// Completion prints a static completion script for the named shell —
// hand-rolled, no dependency.
func Completion(argv []string, w io.Writer) error {
	if len(argv) != 1 {
		return fmt.Errorf("usage: toolhost completion <bash|zsh|fish>")
	}
	var names []string
	for _, v := range Verbs {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	list := strings.Join(names, " ")
	switch argv[0] {
	case "bash":
		fmt.Fprintf(w, `_toolhost() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    COMPREPLY=($(compgen -W "%s" -- "$cur"))
}
complete -F _toolhost toolhost
`, list)
	case "zsh":
		fmt.Fprintf(w, `#compdef toolhost
_toolhost() {
    local -a cmds
    cmds=(%s)
    _describe 'command' cmds
}
_toolhost "$@"
`, list)
	case "fish":
		for _, n := range names {
			fmt.Fprintf(w, "complete -c toolhost -n '__fish_use_subcommand' -a %s\n", n)
		}
	default:
		return fmt.Errorf("unknown shell %q — bash|zsh|fish", argv[0])
	}
	return nil
}
