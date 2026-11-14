"""Old-vs-new trial of Recarto's MCP tool definitions.

Each task goes to a fresh `claude --bare -p` agent whose only tools are one Recarto
MCP server: once with the old binary, once with the new. See
2026-11-14-tool-definitions.md for the setup and the results.

Usage:
  python3 -I 2026-11-14-tool-definitions-trial.py --old BIN --new BIN --snapshot DIR --out DIR \
      [--model claude-sonnet-5-5] [--max-turns 30] [--tasks T01,T02]

Needs APP_ANTHROPIC_API_KEY; it is passed to claude only, as ANTHROPIC_API_KEY.
"""

import argparse
import json
import os
import re
import shutil
import subprocess
import sys

TASKS = [
    ("T01", "search_graph",
     "Which function in this repository decides which served repository a Recarto MCP tool call operates on, "
     "given the optional repo argument? Name the function and its file.",
     "repoTarget, internal/mcp/target.go", [r"\brepoTarget\b", r"target\.go"], []),
    ("T02", "get_code_snippet",
     "How long does a Recarto code-graph tool call wait for an in-flight graph refresh before it answers from "
     "the data it already has? Name the variable that sets it and its value.",
     "freshWait = 3 * time.Second (internal/mcp/graph.go)",
     [r"\bfreshWait\b", r"\b3\s*(\*\s*time\.Second|s\b|sec)"], []),
    ("T03", "get_file_outline",
     "List every method of the App type that is defined in internal/app/app.go.",
     "SignalSync, Close, AllRepos, HasSync, SyncInterval, SyncAll, writeMeta",
     [r"\b%s\b" % n for n in ("SignalSync", "Close", "AllRepos", "HasSync", "SyncInterval", "SyncAll", "writeMeta")],
     [r"DeadWorktrees"]),
    ("T04", "trace_call_path (callers)",
     "Which functions call ensureFresh? List all of them.",
     "searchGraph, traceCallPath, getArchitecture, queryGraph, searchContent, getCodeSnippet, getFileOutline, "
     "findImplementations, findAnnotated, findReferences, getGraphDelta",
     [r"\b%s\b" % n for n in ("searchGraph", "traceCallPath", "getArchitecture", "queryGraph", "searchContent",
                               "getCodeSnippet", "getFileOutline", "findImplementations", "findAnnotated",
                               "findReferences", "getGraphDelta")], []),
    ("T05", "trace_call_path (callees)",
     "Starting from the Serve function in internal/mcp/server.go, which functions of this repository does it "
     "reach within two calls?",
     "serve; then HasSync, runSyncLoop, NewServer (Run is the MCP SDK's method)",
     [r"\bserve\b", r"\bHasSync\b", r"\brunSyncLoop\b", r"\bNewServer\b"], []),
    ("T06", "find_references",
     "Which non-test functions call the memory engine's CompleteTodo method? Give each function and its file.",
     "todoDone (internal/cli/todo.go), completeTodo (internal/mcp/handlers.go)",
     [r"\btodoDone\b", r"\bcompleteTodo\b"], []),
    ("T07", "find_implementations (down)",
     "Which types in the C# test fixtures implement or extend the IShape interface directly?",
     "Circle (record), Margin (struct), IResizable (interface)",
     [r"\bCircle\b", r"\bMargin\b", r"\bIResizable\b"], []),
    ("T08", "find_implementations (up)",
     "Which interface does the C# fixture class CachedRepository implement?",
     "Fixture.Core.Storage.IRepository (IRepository<string>)", [r"\bIRepository\b"], []),
    ("T09", "find_annotated",
     "Which C# symbols in the test fixtures are marked with the Obsolete attribute?",
     "Fixture.App.CachedRepository, Fixture.Core.Geometry.GeoMath.LegacyDistance",
     [r"\bCachedRepository\b", r"\bLegacyDistance\b"], []),
    ("T10", "query_graph",
     "List every C# record type declared in the repository.",
     "GeoPoint, Pixel, Circle, Shape, SyncReport",
     [r"\b%s\b" % n for n in ("GeoPoint", "Pixel", "Circle", "Shape", "SyncReport")], []),
    ("T11", "search_content (unindexed file)",
     "Which version of golangci-lint does the repository's provisioning script install?",
     "2.15.0 (.kanship/setup.sh)", [r"2\.15\.0"], []),
    ("T12", "search_content (literal)",
     'Which function returns the error message "pass symbol (one) or symbols (batch)"?',
     "getCodeSnippet (internal/mcp/graph.go)", [r"\bgetCodeSnippet\b"], []),
    ("T13", "get_architecture",
     "In the module map of this repository, which module does the tools module depend on, and how many "
     "references does that dependency carry?",
     "internal, 31", [r"\binternal\b", r"\b31\b"], []),
    ("T14", "get_graph_delta",
     "Which symbols were added to the code since the last working session in this repository?",
     "TrialDeltaProbe, TrialDeltaMarker (internal/trialdelta/trialdelta.go)",
     [r"\bTrialDeltaProbe\b", r"\bTrialDeltaMarker\b"], []),
]

PROMPT = """Answer this question about the repository in the current directory, using the tools you have.
End your reply with a last line that starts with "ANSWER:" and holds only the answer.

Question: {question}"""

TOOL_NAMES = sorted("mcp__recarto__" + n for n in (
    "record_memory", "record_bug", "recall_memory", "search_bugs", "get_memory", "list_memory", "complete_todo",
    "forget_memory", "search_graph", "trace_call_path", "get_architecture", "query_graph", "search_content",
    "get_code_snippet", "get_file_outline", "find_implementations", "find_annotated", "find_references",
    "get_graph_delta", "report_recarto_issue"))

DELTA_GO = """package trialdelta

// TrialDeltaMarker exists only for the tool-definition trial.
type TrialDeltaMarker struct{}

// TrialDeltaProbe exists only for the tool-definition trial.
func TrialDeltaProbe() int { return 1 }
"""

ARMS = ("old", "new")


def clean_env():
    drop = ("RECARTO_", "CLAUDE_CODE_", "ANTHROPIC_")
    return {k: v for k, v in os.environ.items() if not k.startswith(drop) and k != "CLAUDECODE"}


def recarto_env(out, cache):
    return dict(clean_env(), RECARTO_CACHE_DIR=cache, RECARTO_CONFIG=os.path.join(out, "no-config.toml"))


def step(args, cwd, env, stdin=""):
    p = subprocess.run(args, cwd=cwd, env=env, input=stdin, text=True, capture_output=True)
    if p.returncode != 0:
        sys.exit("%s failed (%d):\n%s%s" % (" ".join(args), p.returncode, p.stdout, p.stderr))
    return p.stdout


def prepare(bins, snapshot, out):
    """Stage the same known get_graph_delta answer in a fresh cache per arm."""
    dirs = {arm: os.path.join(out, arm, "prepared") for arm in ARMS}
    there = [arm for arm in ARMS if os.path.isdir(dirs[arm])]
    if len(there) == len(ARMS):
        return dirs
    if there:
        sys.exit("only %s has a prepared cache: start again with a clean --out" % there)
    delta_dir = os.path.join(snapshot, "internal", "trialdelta")
    if os.path.exists(delta_dir):
        sys.exit("%s exists: use a fresh snapshot worktree" % delta_dir)
    envs = {arm: recarto_env(out, dirs[arm]) for arm in ARMS}
    delta_go = os.path.join(delta_dir, "trialdelta.go")
    # A file's first index journals nothing, so the baseline index must already know the file.
    os.makedirs(delta_dir)
    with open(delta_go, "w") as f:
        f.write("package trialdelta\n")

    def each(args, stdin=""):
        return {arm: step([bins[arm]] + args, snapshot, envs[arm], stdin) for arm in ARMS}

    hook_in = lambda sid, start: json.dumps(dict({"session_id": sid, "cwd": snapshot},
                                                 **({"source": "startup"} if start else {})))
    # Index before the first session so the initial index stays out of the baseline window.
    each(["index"])
    each(["hook", "session-start", "--agent", "claude"], hook_in("trial-baseline", True))
    # One write gives both caches the same file mtime.
    with open(delta_go, "w") as f:
        f.write(DELTA_GO)
    # --force: the freshness token tracks git state, which an untracked file's edit leaves unchanged.
    each(["index", "--force"])
    each(["hook", "stop", "--agent", "claude"], hook_in("trial-baseline", False))
    now = each(["hook", "session-start", "--agent", "claude"], hook_in("trial-now", True))
    for arm in ARMS:
        with open(os.path.join(out, arm, "prepare.txt"), "w") as f:
            f.write(now[arm])
        if "TrialDeltaProbe" not in now[arm] or "TrialDeltaMarker" not in now[arm]:
            sys.exit("%s: the staged delta does not show at session start; see prepare.txt" % arm)
    return dirs


def read_jsonl(path):
    msgs = []
    if os.path.exists(path):
        with open(path) as f:
            for line in f:
                line = line.strip()
                if line.startswith("{"):
                    msgs.append(json.loads(line))
    return msgs


def result_of(msgs):
    for m in reversed(msgs):
        if m.get("type") == "result":
            return m
    return None


def answered(msgs):
    r = result_of(msgs)
    return r is not None and not r.get("is_error") and "ANSWER:" in (r.get("result") or "")


def run_one(task, arm, binary, prepared, args, key, out):
    tid, _, question = task[0], task[1], task[2]
    arm_dir = os.path.join(out, arm)
    path = os.path.join(arm_dir, tid + ".jsonl")
    if result_of(read_jsonl(path)) is not None:
        return
    retried = False
    for attempt in (1, 2):
        cache = os.path.join(arm_dir, "run-cache")
        shutil.rmtree(cache, ignore_errors=True)
        shutil.copytree(prepared, cache)
        cfg = os.path.join(arm_dir, "mcp.json")
        with open(cfg, "w") as f:
            json.dump({"mcpServers": {"recarto": {
                "type": "stdio", "command": binary, "args": ["serve"],
                "env": {"RECARTO_CACHE_DIR": cache, "RECARTO_CONFIG": os.path.join(out, "no-config.toml")}}}}, f)
        env = dict(clean_env(), ANTHROPIC_API_KEY=key, ENABLE_TOOL_SEARCH="false")
        cmd = ["claude", "--bare", "-p", PROMPT.format(question=question), "--model", args.model,
               "--mcp-config", cfg, "--strict-mcp-config", "--tools", "", "--allowedTools", "mcp__recarto",
               "--max-turns", str(args.max_turns), "--output-format", "stream-json", "--verbose",
               "--no-session-persistence"]
        with open(path, "w") as so, open(os.path.join(arm_dir, tid + ".err"), "w") as se:
            try:
                subprocess.run(cmd, cwd=args.snapshot, env=env, stdin=subprocess.DEVNULL, stdout=so, stderr=se,
                               timeout=15 * 60)
            except subprocess.TimeoutExpired:
                se.write("\ntrial: timed out after 15 minutes\n")
        shutil.rmtree(cache)
        msgs = read_jsonl(path)
        init = next((m for m in msgs if m.get("type") == "system" and m.get("subtype") == "init"), None)
        if init is not None and sorted(init.get("tools", [])) != TOOL_NAMES:
            sys.exit("%s/%s: the agent saw tools %s, want exactly the 20 Recarto tools" % (arm, tid, init.get("tools")))
        r = result_of(msgs)
        # Retry once only when the run ended without an answer for API or infrastructure reasons.
        infra = init is None or r is None or (r.get("is_error") and r.get("subtype") != "error_max_turns")
        if answered(msgs) or not infra or attempt == 2:
            break
        retried = True
        os.replace(path, path + ".attempt1")
    if retried:
        with open(os.path.join(arm_dir, tid + ".retried"), "w") as f:
            f.write("retried once after an API or infrastructure error\n")


def grade(task, msgs):
    required, forbidden = task[4], task[5]
    r = result_of(msgs) or {}
    text = r.get("result") or ""
    answer = text.rsplit("ANSWER:", 1)[1].strip() if "ANSWER:" in text else ""
    solved = bool(answer) and all(re.search(p, answer) for p in required) and \
        not any(re.search(p, answer) for p in forbidden)
    tools = [b["name"].replace("mcp__recarto__", "")
             for m in msgs if m.get("type") == "assistant"
             for b in m.get("message", {}).get("content", []) if b.get("type") == "tool_use"]
    return {"solved": solved, "answer": answer, "tools": tools, "num_turns": r.get("num_turns"),
            "cost_usd": r.get("total_cost_usd"), "subtype": r.get("subtype")}


def cell(s):
    return s.replace("|", "\\|").replace("\n", " ")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--old", required=True)
    ap.add_argument("--new", required=True)
    ap.add_argument("--snapshot", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--model", default="claude-sonnet-5-5")
    ap.add_argument("--max-turns", type=int, default=30)
    ap.add_argument("--tasks")
    args = ap.parse_args()
    key = os.environ.get("APP_ANTHROPIC_API_KEY")
    if not key:
        sys.exit("APP_ANTHROPIC_API_KEY is not set")
    args.snapshot = os.path.abspath(args.snapshot)
    out = os.path.abspath(args.out)
    bins = {"old": os.path.abspath(args.old), "new": os.path.abspath(args.new)}
    for arm in ARMS:
        os.makedirs(os.path.join(out, arm), exist_ok=True)
    tasks = TASKS
    if args.tasks:
        want = set(args.tasks.split(","))
        tasks = [t for t in TASKS if t[0] in want]
    prepared = prepare(bins, args.snapshot, out)
    for t in tasks:
        for arm in ARMS:
            print("running %s %s" % (t[0], arm), flush=True)
            run_one(t, arm, bins[arm], prepared[arm], args, key, out)

    results = {}
    for t in tasks:
        results[t[0]] = {"intended_tool": t[1], "question": t[2], "expected": t[3]}
        for arm in ARMS:
            g = grade(t, read_jsonl(os.path.join(out, arm, t[0] + ".jsonl")))
            g["retried"] = os.path.exists(os.path.join(out, arm, t[0] + ".retried"))
            results[t[0]][arm] = g
    with open(os.path.join(out, "results.json"), "w") as f:
        json.dump(results, f, indent=1)

    print("| ID | intended tool | question | expected answer |")
    print("|---|---|---|---|")
    for t in tasks:
        print("| %s | %s | %s | %s |" % (t[0], t[1], cell(t[2]), cell(t[3])))
    print()
    print("| ID | arm | solved | answer | tools called | turns | cost (USD) |")
    print("|---|---|---|---|---|---|---|")
    for t in tasks:
        for arm in ARMS:
            g = results[t[0]][arm]
            print("| %s | %s | %s | %s | %s | %s | %s |" % (
                t[0], arm, "yes" if g["solved"] else "no", cell(g["answer"]), ", ".join(g["tools"]),
                g["num_turns"], "%.3f" % g["cost_usd"] if g["cost_usd"] is not None else "-"))
    solved = {arm: sum(results[t[0]][arm]["solved"] for t in tasks) for arm in ARMS}
    regressions = [t[0] for t in tasks if results[t[0]]["old"]["solved"] and not results[t[0]]["new"]["solved"]]
    print()
    print("solved by old: %d/%d; solved by new: %d/%d; regressions: %s" % (
        solved["old"], len(tasks), solved["new"], len(tasks), ", ".join(regressions) or "0"))


if __name__ == "__main__":
    main()
