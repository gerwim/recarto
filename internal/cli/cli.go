package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// known reports whether cmd is a CLI command that needs an open App.
func known(cmd string) bool {
	switch cmd {
	case "status", "doctor", "remember", "forget", "recall", "bug", "memory", "serve", "index", "graph", "compact", "snapshot", "todo", "sync":
		return true
	}
	return false
}

// Run dispatches a CLI invocation (args excludes the program name) and writes
// command output to out. It validates the command before opening the per-repo
// App, so unknown commands and `help` never touch the store. `init` configures
// the repo's files and needs no store. Hooks are non-blocking: they open the App
// themselves (only when the event has something to act on), report any error to
// stderr, and Run still returns nil so the agent is never blocked.
func Run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usage(out)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		return usage(out)
	}

	// init operates on the working directory's files; no store needed.
	if cmd == "init" {
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return Init(out, dir, binaryPath(), rest)
	}

	// feedback reads the global feedback store directly — no git/app resolution —
	// so it works from anywhere, including outside a git repo.
	if cmd == "feedback" {
		return Feedback(context.Background(), out, rest)
	}

	// server is a standalone daemon: no git repo, no per-repo App — it holds one
	// store per repo key for whichever repos its clients push/pull.
	if cmd == "server" {
		return ServerCmd(context.Background(), out, rest)
	}

	// export walks every per-repo store under the cache dir (plus the global
	// feedback store) directly — no single git repo to resolve into an App.
	if cmd == "export" {
		return Export(context.Background(), out, rest)
	}

	// import uploads a previously written export archive; it needs a file path
	// and sync server config, not a git repo or per-repo App.
	if cmd == "import" {
		return Import(context.Background(), out, rest)
	}

	// repair rebuilds a possibly-corrupt store. It must run before app.Open,
	// because the situation it exists for is exactly the one where app.Open fails.
	if cmd == "repair" {
		return Repair(out, rest)
	}

	// A hook opens the App itself, and only when its event has something to act
	// on: most tool calls a hook fires for need no git or store at all.
	if cmd == "hook" {
		runHook(context.Background(), os.Stdin, out, rest)
		return nil // hooks never block
	}

	if !known(cmd) {
		return fmt.Errorf("unknown command %q (try: init serve hook status doctor remember forget recall bug feedback memory index graph compact snapshot todo sync server export import repair)", cmd)
	}

	// Only the long-lived serving process pays for the proactive corruption check
	// (rate-limited to once a day by the store's stamp file): it opens once and
	// serves for hours, so it must not discover damage on its first write. Hooks
	// and one-shot commands stay as cheap as before.
	cfg := app.Config{QuickCheck: cmd == "serve"}

	a, err := app.Open(cfg)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx := context.Background()
	switch cmd {
	case "status":
		return Status(ctx, a, out)
	case "doctor":
		return Doctor(ctx, a, out)
	case "remember":
		return Remember(ctx, a, out, rest)
	case "forget":
		return Forget(ctx, a, out, rest)
	case "recall":
		return Recall(ctx, a, out, rest)
	case "bug":
		return Bug(ctx, a, out, rest)
	case "memory":
		return Memory(ctx, a, out, rest)
	case "serve":
		return Serve(ctx, a)
	case "index":
		return Index(ctx, a, out, rest)
	case "graph":
		return Graph(ctx, a, out, rest)
	case "compact":
		return Compact(ctx, a, out, rest)
	case "snapshot":
		return Snapshot(ctx, a, out, rest)
	case "todo":
		return Todo(ctx, a, out, rest)
	case "sync":
		return Sync(ctx, a, out, rest)
	}
	return nil // unreachable: known() gates the switch
}

// binaryPath returns the absolute path of the running recarto binary (so hooks
// and .mcp.json invoke the exact binary), falling back to the bare name.
func binaryPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "recarto"
}

func usage(w io.Writer) error {
	fmt.Fprintln(w, "recarto <command>")
	fmt.Fprintln(w, "  init              configure agents (--agent claude,opencode,copilot  --scope user|repo)")
	fmt.Fprintln(w, "  serve             run the MCP server over stdio")
	fmt.Fprintln(w, "  hook <event>      run an agent hook (session-start|post-read|post-edit|augment|stop|shell) [--agent] [--cmd]")
	fmt.Fprintln(w, "  status            repo id, branch, memory counts, ledger")
	fmt.Fprintln(w, "  doctor            health checks (db, WAL, integrity, git)")
	fmt.Fprintln(w, "  repair            rebuild a corrupt store, salvaging memories (--path <store.db>)")
	fmt.Fprintln(w, "  remember <kind>   record a memory (--title --body --files --symbols --tags --pin --global)")
	fmt.Fprintln(w, "  forget <id>       hard-delete a memory by id")
	fmt.Fprintln(w, "  recall [hint]     ranked, budgeted recall (--files --symbols --kind --limit --budget)")
	fmt.Fprintln(w, "  bug add|search    record or search bug memories")
	fmt.Fprintln(w, "  feedback          triage cross-repo Recarto bug reports (list|show|resolve) — works anywhere")
	fmt.Fprintln(w, "  memory show       render all active memory as markdown (--branch)")
	fmt.Fprintln(w, "  index             build/refresh the code graph for this worktree (--force: full rebuild/heal)")
	fmt.Fprintln(w, "  graph <sub>       query the code graph (search|grep|callers|callees|impls|annotated|outline|arch|query|snippet)")
	fmt.Fprintln(w, "  compact           prune/decay memory + sweep dead worktrees (--force --dry-run --budget-tokens --vacuum)")
	fmt.Fprintln(w, "  snapshot          export|import the committable team memory (.recarto/memory.jsonl)")
	fmt.Fprintln(w, "  todo list|done    list open todos, or mark one done")
	fmt.Fprintln(w, "  update            self-update to the latest (or a chosen) release (--version --list --check --yes)")
	fmt.Fprintln(w, "  sync              manual HTTP memory sync (--pull --push --full); auto-runs at session-start/stop")
	fmt.Fprintln(w, "  server            run the HTTP memory-sync server (--addr --data-dir --tls-cert --tls-key; needs RECARTO_SERVER_KEY)")
	fmt.Fprintln(w, "  export            export every project's full memory history to an archive (--out --repo --push --url --key)")
	fmt.Fprintln(w, "  import [--url --key] <file>  upload a previously written export archive to the sync server")
	return nil
}
