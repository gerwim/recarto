"""Measure the MCP tool definitions a Recarto binary offers.

Usage (from a Recarto checkout):
  python3 -I 2026-11-14-tool-definitions-measure.py BINARY [--several-repos] [--workspace NAME] [--dump-shape FILE]

Sizes: description = characters of `description`; input schema = len(json.dumps(inputSchema));
parameter descriptions = characters of every inputSchema.properties.*.description;
definition = description + input schema.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile


def rpc(proc, msg):
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()


def read_reply(proc, want_id):
    while True:
        line = proc.stdout.readline()
        if not line:
            sys.exit("server closed stdout before replying to request %d" % want_id)
        msg = json.loads(line)
        if msg.get("id") == want_id:
            if "error" in msg:
                sys.exit("request %d failed: %s" % (want_id, msg["error"]))
            return msg["result"]


def list_tools(binary, several_repos, workspace):
    tmp = tempfile.mkdtemp(prefix="recarto-measure-")
    try:
        env = {k: v for k, v in os.environ.items() if not k.startswith("RECARTO_")}
        env["RECARTO_CACHE_DIR"] = os.path.join(tmp, "cache")
        env["RECARTO_CONFIG"] = os.path.join(tmp, "missing-config.toml")
        if workspace:
            env["RECARTO_WORKSPACE"] = workspace
        if several_repos:
            # A separate repository: a worktree of the cwd repo would count as the primary.
            extra = os.path.join(tmp, "extra")
            os.makedirs(extra)
            git_env = dict(env, GIT_AUTHOR_NAME="m", GIT_AUTHOR_EMAIL="m@m",
                           GIT_COMMITTER_NAME="m", GIT_COMMITTER_EMAIL="m@m")
            for args in (["init", "-q"], ["commit", "-q", "--allow-empty", "-m", "init"]):
                subprocess.run(["git"] + args, cwd=extra, env=git_env, check=True)
            env["RECARTO_REPOS"] = extra
        proc = subprocess.Popen([binary, "serve"], env=env, text=True,
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        try:
            rpc(proc, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
                "protocolVersion": "2025-06-18", "capabilities": {},
                "clientInfo": {"name": "measure", "version": "1"}}})
            read_reply(proc, 1)
            rpc(proc, {"jsonrpc": "2.0", "method": "notifications/initialized"})
            rpc(proc, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
            res = read_reply(proc, 2)
        finally:
            proc.kill()
            proc.wait()
        if res.get("nextCursor"):
            sys.exit("tools/list is paged; this script reads only one page")
        return res["tools"]
    finally:
        shutil.rmtree(tmp)


def sizes(tool):
    schema = tool.get("inputSchema", {})
    params = sum(len(p.get("description", "")) for p in schema.get("properties", {}).values())
    desc = len(tool.get("description", ""))
    sch = len(json.dumps(schema))
    return desc, sch, params, desc + sch


def strip_descriptions(v):
    if isinstance(v, dict):
        return {k: strip_descriptions(x) for k, x in v.items() if k != "description"}
    if isinstance(v, list):
        return [strip_descriptions(x) for x in v]
    return v


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("binary")
    ap.add_argument("--several-repos", action="store_true")
    ap.add_argument("--workspace")
    ap.add_argument("--dump-shape")
    args = ap.parse_args()
    tools = list_tools(os.path.abspath(args.binary), args.several_repos, args.workspace)
    print("| tool | description | input schema | parameter descriptions | definition |")
    print("|---|---|---|---|---|")
    total = [0, 0, 0, 0]
    for t in sorted(tools, key=lambda t: t["name"]):
        row = sizes(t)
        total = [a + b for a, b in zip(total, row)]
        print("| %s | %d | %d | %d | %d |" % ((t["name"],) + row))
    print("| **total (%d tools)** | %d | %d | %d | %d |" % ((len(tools),) + tuple(total)))
    if args.dump_shape:
        shape = {t["name"]: strip_descriptions(t.get("inputSchema", {})) for t in tools}
        with open(args.dump_shape, "w") as f:
            json.dump(shape, f, sort_keys=True, indent=1)
            f.write("\n")


if __name__ == "__main__":
    main()
