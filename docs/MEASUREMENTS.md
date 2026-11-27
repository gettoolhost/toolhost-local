# Measurements

Numbers quoted in the README, with the script that produces them. Re-run it before quoting a number anywhere else.

## Tool list size: the filesystem server, direct and through ToolHost Local

What the agent receives from `tools/list` when it connects straight to `@modelcontextprotocol/server-filesystem`, and when it connects through ToolHost Local with every tool approved, then with two approved (`fs__read_text_file`, `fs__list_directory`).

Size is the byte length of `result.tools` re-serialised as compact JSON, so transport framing and whitespace don't count.

Reproduce:

```bash
scripts/measure-tools-list.sh          # server-filesystem 2.4.0 by default
scripts/measure-tools-list.sh 2.5.0    # or any other version
```

It needs Go (or `TOOLHOST=/path/to/toolhost`), Node with `npx`, `curl` and `python3`. It works in a fresh temp directory with its own `TOOLHOST_HOME`, so your own config is never read or changed.

Recorded run:

```text
toolhost:  toolhost 0.0.4 (v0.0.3-14-g31168ff-dirty)
server:    @modelcontextprotocol/server-filesystem@2.4.0
date:      2026-11-27T05:03:58Z
size:      bytes of result.tools as compact JSON

direct to the server       16 server tools =  19369 B | 0 gateway__ verbs =     0 B | total  19369 B
toolhost, all approved     16 server tools =  19433 B | 5 gateway__ verbs =  2570 B | total  22002 B
toolhost, 2 approved        2 server tools =   2366 B | 5 gateway__ verbs =  2570 B | total   4935 B
```

Reading it:

- Direct, the agent gets 16 tools, 19.4 KB. Through ToolHost Local with two approved, it gets those 2 plus the 5 `gateway__` verbs, 4.9 KB.
- With everything approved, ToolHost Local adds 64 bytes of `fs__` prefixes and the 2.6 KB of verbs.
- The server tools' sizes are set by the server, so they change with its version. The verbs' size is set by ToolHost Local, so it changes with ours.
