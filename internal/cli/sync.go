package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/features"
	"git.gerwim.nl/gerwim/recarto/internal/remotesync"
)

// Sync runs a manual pull and/or push against the configured HTTP memory-sync
// server. With neither --pull nor --push given, it does both (the same
// pull-then-push order the session-start/Stop hooks use). It covers every
// configured store: the repo store, every served extra repo (RECARTO_REPOS),
// and — when RECARTO_WORKSPACE is set — the cross-repo workspace store, so the
// manual/debug tool converges exactly what the hooks do. A non-git directory
// has no repo key but can still sync its extra-repo and workspace stores.
func Sync(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	pull := fs.Bool("pull", false, "pull only")
	push := fs.Bool("push", false, "push only")
	full := fs.Bool("full", false, "reset the local pull cursor first, forcing a full re-pull")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	type target struct {
		label string
		s     *remotesync.Syncer
	}
	var targets []target
	if a.Sync != nil {
		targets = append(targets, target{"repo", a.Sync})
	}
	for _, rc := range a.Repos {
		if rc.Sync != nil {
			targets = append(targets, target{"repo " + rc.Git.RepoName, rc.Sync})
		}
	}
	if a.Workspace != nil && a.Workspace.Sync != nil {
		targets = append(targets, target{"workspace " + a.Workspace.Name, a.Workspace.Sync})
	}
	if !a.Features.On(features.Sync) {
		fmt.Fprintln(w, "sync: off (recarto config enable sync turns it on)")
		return nil
	}
	if len(targets) == 0 {
		fmt.Fprintln(w, "sync: not configured (set RECARTO_SYNC_URL and RECARTO_SYNC_KEY; "+
			"this repo also needs a resolvable root commit, or RECARTO_SYNC_REPO)")
		return nil
	}

	doPull, doPush := *pull, *push
	if !doPull && !doPush {
		doPull, doPush = true, true
	}
	for _, t := range targets {
		fmt.Fprintf(w, "sync: %s key %s\n", t.label, t.s.RepoKeyValue())
		if *full {
			if err := t.s.ResetCursor(ctx); err != nil {
				return err
			}
			fmt.Fprintf(w, "sync: %s cursor reset; next pull reconciles from scratch\n", t.label)
		}
		if doPull {
			if err := t.s.Pull(ctx); err != nil {
				return fmt.Errorf("%s pull: %w", t.label, err)
			}
			fmt.Fprintf(w, "%s pull: ok\n", t.label)
		}
		if doPush {
			if err := t.s.Push(ctx); err != nil {
				return fmt.Errorf("%s push: %w", t.label, err)
			}
			fmt.Fprintf(w, "%s push: ok\n", t.label)
		}
	}
	return nil
}
