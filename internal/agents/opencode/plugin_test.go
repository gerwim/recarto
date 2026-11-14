package opencode

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// stubRecarto stands in for the recarto binary: it logs "<event> <payload>" and
// answers each hook with canned neutral JSON.
const stubRecarto = `#!/bin/sh
payload=$(cat)
printf '%s %s\n' "$2" "$payload" >> "$(dirname "$0")/calls.log"
case "$2" in
  session-start) echo '{"additionalContext":"CTX-START"}' ;;
  stop) echo '{"decision":"block","reason":"NUDGE"}' ;;
  *) echo "{\"additionalContext\":\"CTX-$2\"}" ;;
esac
`

// pluginDriver loads the generated plugin, drives its OpenCode 2 setup() with a
// fake context and its OpenCode 1 server() hooks, and prints what they did.
const pluginDriver = `import plugin from "./recarto.mjs";
const dir = process.argv[2];
const hooks = {};
const synthetic = [];
let finishTurn;
const turnDone = new Promise((r) => (finishTurn = r));
const register = (domain) => async (name, fn) => { hooks[domain + "." + name] = fn; return { dispose: async () => {} }; };
const ctx = {
  location: { directory: dir },
  session: {
    hook: register("session"),
    get: async ({ sessionID }) => ({ id: sessionID, location: { directory: dir }, ...(sessionID === "child" ? { parentID: "root" } : {}) }),
    synthetic: async (input) => { synthetic.push(input); },
  },
  tool: { hook: register("tool") },
  event: {
    subscribe: async function* () {
      await turnDone;
      yield { type: "session.execution.succeeded", data: { sessionID: "child" } };
      yield { type: "session.execution.succeeded", data: { sessionID: "root" } };
    },
  },
};
const cleanup = await plugin.setup(ctx);

const r1 = { sessionID: "root", system: [] };
const r2 = { sessionID: "root", system: [] };
await hooks["session.context"](r1);
await hooks["session.context"](r2);

const after = (tool, input, result) => ({ tool, sessionID: "root", status: "completed", input, result });
const read = after("read", { path: "a.go", offset: 10, limit: 5 }, { content: [{ type: "text", text: "file body" }] });
const listing = after("read", { path: "." }, { content: "listing", output: { type: "list-page" } });
const grep = after("grep", { pattern: "Foo" }, { content: "1 match" });
const edit = after("edit", { path: "b.go" }, { content: [{ type: "text", text: "Edited" }] });
const patch = after("patch", { patchText: "*** Begin Patch" }, { content: "ok", output: { applied: [{ target: "/abs/c.go" }, { target: "/abs/d.go" }] } });
const web = after("websearch", { query: "Foo" }, { content: "web" });
const failed = { tool: "read", sessionID: "root", status: "error", input: { path: "a.go" }, error: {} };
const bash = after("bash", { command: "rg Foo | head", workdir: "sub" }, { content: "matches" });
const shell = after("shell", { command: "cat a.go" }, { content: [{ type: "text", text: "body" }] });
const build = after("bash", { command: "go build ./..." }, { content: "built" });
for (const e of [read, listing, grep, edit, patch, web, failed, bash, shell, build]) await hooks["tool.execute.after"](e);
finishTurn();

for (let i = 0; i < 250 && synthetic.length === 0; i++) await new Promise((r) => setTimeout(r, 20));
await cleanup();

const v1 = await plugin.server({ directory: dir });
const sys = { system: [] };
await v1["experimental.chat.system.transform"]({ sessionID: "v1s" }, sys);
const v1read = { title: "", output: "body", metadata: {} };
await v1["tool.execute.after"]({ tool: "read", sessionID: "v1s", callID: "c", args: { filePath: "e.go", offset: 3 } }, v1read);
const v1bash = { title: "", output: "hits", metadata: {} };
await v1["tool.execute.after"]({ tool: "bash", sessionID: "v1s", callID: "d", args: { command: "grep -rn Foo .", workdir: "/abs/w" } }, v1bash);
const v1test = { title: "", output: "ok", metadata: {} };
await v1["tool.execute.after"]({ tool: "bash", sessionID: "v1s", callID: "e", args: { command: "npm test" } }, v1test);
await v1.event({ event: { type: "file.edited", properties: { file: "/abs/f.go" } } });

console.log(JSON.stringify({
  id: plugin.id, r1: r1.system, r2: r2.system,
  read: read.result.content, listing: listing.result.content, grep: grep.result.content,
  edit: edit.result.content, patch: patch.result.content, web: web.result.content,
  bash: bash.result.content, shell: shell.result.content, build: build.result.content,
  synthetic, v1sys: sys.system, v1read: v1read.output, v1bash: v1bash.output, v1test: v1test.output,
}));
`

// TestPluginHidesWindowsConsole checks the shared launcher without Node or a
// POSIX shell, so the console-window regression is covered on Windows too.
func TestPluginHidesWindowsConsole(t *testing.T) {
	js := pluginJS("recarto")
	const launcher = `spawn(BIN, ["hook", event, "--agent", "opencode"], { cwd, stdio: ["pipe", "pipe", "ignore"], windowsHide: true })`
	if !strings.Contains(js, launcher) {
		t.Fatal("generated hook launcher must pass windowsHide: true to spawn")
	}
}

// TestPluginRunsUnderNode executes the generated plugin (as OpenCode's JS runtime
// would) against a stub recarto, covering both plugin APIs end to end.
func TestPluginRunsUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs node and a POSIX shell")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "recarto")
	for name, body := range map[string]string{"recarto": stubRecarto, "recarto.mjs": pluginJS(stub), "driver.mjs": pluginDriver} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(node, filepath.Join(dir, "driver.mjs"), dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("driver output %q: %v", raw, err)
	}
	text := func(s ...string) []any {
		parts := make([]any, len(s))
		for i, v := range s {
			parts[i] = map[string]any{"type": "text", "text": v}
		}
		return parts
	}
	want := map[string]any{
		"id":        "recarto",
		"r1":        text("CTX-START"),
		"r2":        text("CTX-START"),
		"read":      text("file body", "CTX-post-read"),
		"listing":   "listing", // directory reads are not file reads
		"grep":      text("1 match", "CTX-augment"),
		"edit":      text("Edited", "CTX-post-edit"),
		"patch":     text("ok", "CTX-post-edit\nCTX-post-edit"),
		"web":       "web", // websearch is not a code search
		"bash":      text("matches", "CTX-shell"),
		"shell":     text("body", "CTX-shell"),
		"build":     "built", // no search or view: recarto never starts
		"synthetic": []any{map[string]any{"sessionID": "root", "text": "NUDGE"}},
		"v1sys":     []any{"CTX-START"},
		"v1read":    "body\n\nCTX-post-read",
		"v1bash":    "hits\n\nCTX-shell",
		"v1test":    "ok",
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}

	log, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		event, payload, _ := strings.Cut(line, " ")
		var p map[string]any
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		fields := []string{event}
		for _, k := range slices.Sorted(maps.Keys(p)) {
			fields = append(fields, fmt.Sprintf("%s=%v", k, p[k]))
		}
		calls = append(calls, strings.ReplaceAll(strings.Join(fields, " "), dir, "<dir>"))
	}
	wantCalls := []string{
		"session-start session_id=root", // once for two requests of the session
		"post-read file_path=<dir>/a.go limit=5 offset=10 session_id=root tool_result=file body",
		"augment pattern=Foo session_id=root",
		"post-edit file_path=<dir>/b.go session_id=root",
		"post-edit file_path=/abs/c.go session_id=root",
		"post-edit file_path=/abs/d.go session_id=root",
		"shell command=rg Foo | head cwd=<dir>/sub session_id=root",
		"shell command=cat a.go cwd=<dir> session_id=root",
		"stop session_id=root", // the subagent session is skipped
		"session-start session_id=v1s",
		"post-read file_path=<dir>/e.go offset=3 session_id=v1s tool_result=body",
		"shell command=grep -rn Foo . cwd=/abs/w session_id=v1s",
		"post-edit file_path=/abs/f.go",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("recarto calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(wantCalls, "\n"))
	}
}
