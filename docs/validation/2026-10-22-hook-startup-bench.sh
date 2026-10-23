#!/usr/bin/env bash
# Hook start-up benchmark behind 2026-10-22-hook-startup.md.
#
#   docs/validation/2026-10-22-hook-startup-bench.sh <base-recarto> <new-recarto> <out-dir>
#
# Run from inside the Recarto repo; needs hyperfine, taskset, python3 and node.
# Every figure is the median of 10 timed runs after 3 warm-ups, pinned to CPUs
# 0-1 (a 2-CPU budget, GOMAXPROCS follows). Each binary gets its own cache dir,
# indexed and seeded identically; every timed run gets a fresh session id
# (prepared, untimed), so each one is a first read/edit/search of its session.
# "configured" adds RECARTO_REPOS (a second clone), RECARTO_WORKSPACE and HTTP
# sync against a local `recarto server` (one per binary, so neither pulls the
# other's memories).
set -euo pipefail
BASE=$(realpath "$1") NEW=$(realpath "$2") OUT=$(realpath -m "$3")
REPO=$(git rev-parse --show-toplevel)
FILE=$REPO/internal/agents/handle.go
W=$(mktemp -d)
mkdir -p "$OUT"
cd "$REPO"
unset RECARTO_REPOS RECARTO_WORKSPACE RECARTO_SYNC_URL RECARTO_SYNC_KEY RECARTO_SYNC_REPO
git clone -q "$REPO" "$W/second"

RECARTO_SERVER_KEY=benchkey "$BASE" server --addr 127.0.0.1:18765 --data-dir "$W/sync-base" >/dev/null 2>&1 &
S1=$!
RECARTO_SERVER_KEY=benchkey "$NEW" server --addr 127.0.0.1:18766 --data-dir "$W/sync-new" >/dev/null 2>&1 &
S2=$!
trap 'kill $S1 $S2' EXIT

SYNC=
plain() {
	unset RECARTO_REPOS RECARTO_WORKSPACE RECARTO_SYNC_URL RECARTO_SYNC_KEY
	SYNC=
}
configured() {
	export RECARTO_REPOS=$W/second RECARTO_WORKSPACE=bench RECARTO_SYNC_KEY=benchkey
	SYNC=1
}
bin() { if [ "$1" = base ]; then echo "$BASE"; else echo "$NEW"; fi; }
# use <base|new>: point the environment at that binary's cache and sync server.
use() {
	export RECARTO_CACHE_DIR=$W/cache-$1
	if [ -n "$SYNC" ]; then
		if [ "$1" = base ]; then export RECARTO_SYNC_URL=http://127.0.0.1:18765; else export RECARTO_SYNC_URL=http://127.0.0.1:18766; fi
	fi
}

# Payload templates (__SESSION__ is replaced per run).
python3 - "$W" "$REPO" "$FILE" <<'EOF'
import json, sys
w, repo, f = sys.argv[1:]
text, S = open(f).read(), "__SESSION__"
payloads = {
    "session-start": {"session_id": S, "source": "startup"},
    "post-read": {"session_id": S, "tool_name": "Read", "tool_input": {"file_path": f},
                  "tool_response": {"type": "text", "file": {"filePath": f, "content": text}}},
    "post-edit": {"session_id": S, "tool_name": "Edit", "tool_input": {"file_path": f}},
    "search": {"session_id": S, "tool_name": "Grep", "tool_input": {"pattern": "RunHook"}},
    "stop": {"session_id": S, "stop_hook_active": False},
    "empty-pattern": {"session_id": S, "tool_name": "Grep", "tool_input": {"pattern": ""}},
    "outside-edit": {"session_id": S, "tool_name": "Edit", "tool_input": {"file_path": "/opt/elsewhere/notes.go"}},
    "copilot-view": {"sessionId": S, "toolName": "view", "toolArgs": {"path": f},
                     "toolResult": {"resultType": "success", "textResultForLlm": text}},
    "copilot-edit": {"sessionId": S, "toolName": "edit", "toolArgs": {"path": f, "old_str": "a", "new_str": "a"}},
    "copilot-grep": {"sessionId": S, "toolName": "grep", "toolArgs": {"pattern": "RunHook", "path": repo}},
    "copilot-bash": {"sessionId": S, "toolName": "bash", "toolArgs": {"command": "go test ./...", "description": "Run tests"}},
}
for name, p in payloads.items():
    json.dump(p, open(f"{w}/{name}.tmpl", "w"))
EOF

# prep <template> [<recarto>]: fresh-session payload; with a binary, also opens
# that session (so a timed stop closes a real session row).
cat >"$W/prep" <<EOF
#!/usr/bin/env bash
s=bench-\$(date +%s%N)
sed "s/__SESSION__/\$s/" "$W/\$1.tmpl" >"$W/payload.json"
if [ -n "\${2:-}" ]; then sed "s/__SESSION__/\$s/" "$W/session-start.tmpl" | "\$2" hook session-start >/dev/null; fi
EOF
# copilot <old|new|ignored> <recarto> <tool>: the hook entries Copilot starts
# for one postToolUse of <tool>, each its own `bash -c` as Copilot runs them.
# old: the former fan-out (post-edit, post-read, augment for every tool); new:
# the one entry matched to the tool (none for bash); ignored: all three new
# entries, as Copilot before v1.0.63 (which ignored postToolUse matchers) runs them.
cat >"$W/copilot" <<EOF
#!/usr/bin/env bash
case \$1 in
old) events="post-edit post-read augment" ;;
ignored) events="post-read post-edit augment" ;;
new) case \$3 in view) events=post-read ;; edit) events=post-edit ;; grep) events=augment ;; *) events= ;; esac ;;
esac
for e in \$events; do bash -c "'\$2' hook \$e --agent copilot" <"$W/payload.json" >/dev/null; done
EOF
chmod +x "$W/prep" "$W/copilot"
: >"$W/payload.json" # hyperfine checks --input exists before the first prepare

# measure <env> <row> <prepare-args> <command…>: one hyperfine per binary.
measure() {
	local env=$1 row=$2 prep=$3
	shift 3
	for b in base new; do
		use $b
		local p=${prep//@BIN@/$(bin $b)} c=${*//@BIN@/$(bin $b)}
		taskset -c 0,1 hyperfine -N --style none --warmup 3 --runs 10 \
			--prepare "$W/prep $p" --input "$W/payload.json" \
			--export-json "$OUT/$env--$row--$b.json" "$c" >/dev/null
	done
	echo "measured $env $row" >&2
}

configured
for b in base new; do # seed both caches identically
	use $b
	(cd "$W/second" && "$(bin $b)" index >/dev/null 2>&1)
	"$(bin $b)" index >/dev/null 2>&1
	"$(bin $b)" remember bug --title "bench: decode the hook payload first" \
		--body "Bench seed: a bug anchored to handle.go so read/edit/search surface it." \
		--files internal/agents/handle.go --symbols RunHook >/dev/null
	"$(bin $b)" remember decision --title "bench: hooks stay exit-0" \
		--body "Bench seed: guidance anchored to handle.go." --files internal/agents/handle.go >/dev/null
	"$(bin $b)" remember todo --title "bench: open todo on handle.go" \
		--body "Bench seed: a todo anchored to handle.go." --files internal/agents/handle.go >/dev/null
done

for env in plain configured; do
	$env
	measure $env session-start session-start "@BIN@ hook session-start --agent claude"
	measure $env post-read post-read "@BIN@ hook post-read --agent claude"
	measure $env post-edit post-edit "@BIN@ hook post-edit --agent claude"
	measure $env search search "@BIN@ hook augment --agent claude"
	measure $env stop "stop @BIN@" "@BIN@ hook stop --agent claude"
	measure $env empty-pattern empty-pattern "@BIN@ hook augment --agent claude"
	measure $env outside-edit outside-edit "@BIN@ hook post-edit --agent claude"
	for tool in view edit grep bash; do
		for mode in old new ignored; do
			for b in base new; do
				[ $mode != old ] && [ $b = base ] && continue # base only knows the fan-out
				[ $mode = old ] && [ $b = new ] && continue
				use $b
				taskset -c 0,1 hyperfine -N --style none --warmup 3 --runs 10 \
					--prepare "$W/prep copilot-$tool" \
					--export-json "$OUT/$env--copilot-$tool-$mode--$b.json" "$W/copilot $mode $(bin $b) $tool" >/dev/null
			done
		done
		echo "measured $env copilot $tool" >&2
	done
	use new
	taskset -c 0,1 hyperfine -N --style none --warmup 3 --runs 10 --export-json "$OUT/$env--version--new.json" "$NEW version" >/dev/null
	# OpenCode: drive the generated plugin's OpenCode 1 hooks from node.
	for b in base new; do
		use $b
		mkdir -p "$W/oc-$b" && (cd "$W/oc-$b" && "$(bin $b)" init --agent opencode --scope repo >/dev/null)
		taskset -c 0,1 node --input-type=module - "$W/oc-$b/.opencode/plugins/recarto.js" "$REPO" "$FILE" >"$OUT/$env--opencode--$b.jsonl" <<'EOF'
const [plugin, dir, file] = process.argv.slice(2);
const { readFileSync } = await import("node:fs");
const text = readFileSync(file, "utf8");
const h = await (await import(plugin)).default.server({ directory: dir });
const id = (op, i) => `oc-${op}-${process.hrtime.bigint()}-${i}`;
const ops = {
  "session-start": (i) => h["experimental.chat.system.transform"]({ sessionID: id("s", i) }, { system: [] }),
  "post-read": (i) => h["tool.execute.after"]({ tool: "read", sessionID: id("r", i), args: { filePath: file } }, { output: text }),
  "post-edit": () => h.event({ event: { type: "file.edited", properties: { file } } }),
  "search": (i) => h["tool.execute.after"]({ tool: "grep", sessionID: id("g", i), args: { pattern: "RunHook" } }, { output: "" }),
  "empty-pattern": (i) => h["tool.execute.after"]({ tool: "grep", sessionID: id("e", i), args: { pattern: "" } }, { output: "" }),
};
for (const [name, fn] of Object.entries(ops)) {
  for (let i = 0; i < 3; i++) await fn(`w${i}`);
  const xs = [];
  for (let i = 0; i < 10; i++) {
    const t = process.hrtime.bigint();
    await fn(i);
    xs.push(Number(process.hrtime.bigint() - t) / 1e6);
  }
  xs.sort((a, b) => a - b);
  console.log(JSON.stringify({ name, median_ms: (xs[4] + xs[5]) / 2 }));
}
EOF
	done
	echo "measured $env opencode" >&2
done

# A real session also runs `recarto serve` (the MCP server), whose pooled
# connection keeps the primary store open: a hook is then never the store's
# last connection and skips SQLite's WAL checkpoint (and its fsyncs) on close.
plain
SERVES=()
for b in base new; do
	use $b
	mkfifo "$W/stdin-$b"
	"$(bin $b)" serve <"$W/stdin-$b" >/dev/null 2>&1 &
	SERVES+=($!)
	exec {fd}>"$W/stdin-$b" # hold stdin open so serve keeps running
done
trap 'kill $S1 $S2 ${SERVES[*]}' EXIT
sleep 1
for row in session-start post-read post-edit search; do
	measure serve $row $row "@BIN@ hook $( [ $row = search ] && echo augment || echo $row ) --agent claude"
done
measure serve stop "stop @BIN@" "@BIN@ hook stop --agent claude"
