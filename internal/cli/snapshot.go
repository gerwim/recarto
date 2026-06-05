package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.gerwim.nl/gerwim/recarto/internal/app"
	"git.gerwim.nl/gerwim/recarto/internal/snapshot"
)

// Snapshot exports or imports the committable team-memory file.
//
//	recarto snapshot export   # write .recarto/memory.jsonl
//	recarto snapshot import   # merge .recarto/memory.jsonl into the local store
func Snapshot(ctx context.Context, a *app.App, w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recarto snapshot export|import")
	}
	path := filepath.Join(a.Git.Worktree, ".recarto", "memory.jsonl")
	switch args[0] {
	case "export":
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := snapshot.Export(ctx, a.Engine, f)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "exported %d memories to %s\n", n, path)
		return nil
	case "import":
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		ins, rein, skip, err := snapshot.Import(ctx, a.Engine, f)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "imported %s: %d new, %d reinforced, %d skipped\n", path, ins, rein, skip)
		return nil
	default:
		return fmt.Errorf("unknown snapshot subcommand %q (export|import)", args[0])
	}
}
