#!/usr/bin/env bash
# measure-tools-list.sh: reproduce the README's tool-list measurement.
#
# Lists the official filesystem MCP server's tools directly, then through a
# throwaway ToolHost Local twice: with every tool approved, and with two.
# Prints the tool counts and the size of each tools/list result.
#
# Size = bytes of `result.tools` re-serialised as compact JSON
# (Python json.dumps, separators "," and ":"), so transport framing and
# whitespace don't count.
#
# Needs: go (or TOOLHOST=/path/to/toolhost), node with npx, curl, python3.
# ToolHost's files go in a fresh temp dir, which it deletes: TOOLHOST_HOME
# points there, so your own ~/.toolhost is never read or touched. npx and
# go still use their usual caches (~/.npm, the Go build cache).
#
# Usage: scripts/measure-tools-list.sh [server-filesystem version]
set -euo pipefail

SERVER_VERSION="${1:-2.4.0}"
PKG="@modelcontextprotocol/server-filesystem@${SERVER_VERSION}"
KEEP=(fs__read_text_file fs__list_directory)

repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
serve_pid=""
cleanup() {
    if [ -n "$serve_pid" ]; then kill "$serve_pid" 2>/dev/null || true; fi
    rm -rf "$work"
}
trap cleanup EXIT

export TOOLHOST_HOME="$work/home"
mkdir -p "$work/files"
cd "$work/files" # no project env from the caller's folder can apply

bin="${TOOLHOST:-}"
if [ -z "$bin" ]; then
    bin="$work/toolhost"
    (cd "$repo" && go build -o "$bin" ./cmd/toolhost)
fi

# size LABEL: read one tools/list response (JSON or SSE) on stdin, print counts.
size() {
    python3 -I -c '
import json, sys
raw = sys.stdin.read()
data = [l[5:] for l in raw.splitlines() if l.startswith("data:")]
tools = json.loads(data[0] if data else raw)["result"]["tools"]
b = lambda ts: len(json.dumps(ts, separators=(",", ":"))) if ts else 0
srv = [t for t in tools if not t["name"].startswith("gateway__")]
verbs = [t for t in tools if t["name"].startswith("gateway__")]
print(f"{sys.argv[1]:<26} {len(srv):>2} server tools = {b(srv):>6} B | "
      f"{len(verbs)} gateway__ verbs = {b(verbs):>5} B | total {b(tools):>6} B")
' "$1"
}

# 1. Direct: speak MCP over stdio to the server itself, no gateway.
direct="$(python3 -I - "$PKG" "$work/files" <<'PY'
import json, subprocess, sys
p = subprocess.Popen(["npx", "-y", sys.argv[1], sys.argv[2]], stdin=subprocess.PIPE,
                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
def send(msg):
    p.stdin.write(json.dumps(msg) + "\n"); p.stdin.flush()
send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {},
    "clientInfo": {"name": "measure", "version": "0"}}})
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
for line in p.stdout:
    msg = json.loads(line)
    if msg.get("id") == 2:
        print(line.strip())
        break
p.terminate()
PY
)"

# 2. Through ToolHost Local, on a free loopback port.
port="$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
"$bin" init >/dev/null
cfg="$TOOLHOST_HOME/toolhost.json"
python3 -I - "$cfg" "127.0.0.1:$port" <<'PY'
import json, sys
path, listen = sys.argv[1], sys.argv[2]
c = json.load(open(path))
c["listen"] = listen
json.dump(c, open(path, "w"), indent=2)
PY
token="$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["token"])' "$cfg")"

"$bin" mcp add fs -- npx -y "$PKG" "$work/files" >/dev/null
read -r -a all <<<"$("$bin" discover --json | python3 -I -c 'import json, sys; print(" ".join(t["qualified"] for t in json.load(sys.stdin)))')"
"$bin" approve "${all[@]}" >/dev/null

"$bin" serve >"$work/serve.log" 2>&1 &
serve_pid=$!
for _ in $(seq 1 100); do
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
    sleep 0.2
done

list() {
    curl -fsS -X POST "http://127.0.0.1:$port/mcp" \
        -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
        -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-06-18' \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
}

all_approved="$(list)"

# Revoke all but KEEP while serving; wait for the hot reload to land.
drop=()
for t in "${all[@]}"; do
    case " ${KEEP[*]} " in *" $t "*) ;; *) drop+=("$t") ;; esac
done
"$bin" revoke "${drop[@]}" >/dev/null
for _ in $(seq 1 50); do
    two_approved="$(list)"
    n="$(printf '%s' "$two_approved" | size x | awk '{print $2}')"
    [ "$n" = "${#KEEP[@]}" ] && break
    sleep 0.2
done

echo "toolhost:  $("$bin" version) ($(git -C "$repo" describe --tags --always --dirty 2>/dev/null || echo unknown))"
echo "server:    $PKG"
echo "date:      $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "size:      bytes of result.tools as compact JSON"
echo
printf '%s' "$direct" | size "direct to the server"
printf '%s' "$all_approved" | size "toolhost, all approved"
printf '%s' "$two_approved" | size "toolhost, 2 approved"
