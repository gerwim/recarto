package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Sync runs a manual pull and/or push against the configured HTTP memory-sync
// server. With neither --pull nor --push given, it does both (the same
// pull-then-push order the session-start/Stop hooks use).
func Sync(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	pull := fs.Bool("pull", false, "pull only")
	push := fs.Bool("push", false, "push only")
	full := fs.Bool("full", false, "reset the local pull cursor first, forcing a full re-pull")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	if a.Sync == nil {
		fmt.Fprintln(w, "sync: not configured (set RECARTO_SYNC_URL and RECARTO_SYNC_KEY; "+
			"this repo also needs a resolvable root commit, or RECARTO_SYNC_REPO)")
		return nil
	}
	fmt.Fprintf(w, "sync: repo key %s\n", a.Sync.RepoKeyValue())

	if *full {
		if err := a.Sync.ResetCursor(ctx); err != nil {
			return err
		}
		fmt.Fprintln(w, "sync: cursor reset; next pull reconciles from scratch")
	}

	doPull, doPush := *pull, *push
	if !doPull && !doPush {
		doPull, doPush = true, true
	}
	if doPull {
		if err := a.Sync.Pull(ctx); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		fmt.Fprintln(w, "pull: ok")
	}
	if doPush {
		if err := a.Sync.Push(ctx); err != nil {
			return fmt.Errorf("push: %w", err)
		}
		fmt.Fprintln(w, "push: ok")
	}
	return nil
}
