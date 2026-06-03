package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"git.gerwim.nl/gerwim/recarto/internal/app"
)

// Doctor runs health checks: store reachable, WAL enabled, integrity ok, a schema
// version applied, and git identity resolved. It returns an error if any fail so
// the process exits non-zero.
func Doctor(ctx context.Context, a *app.App, w io.Writer) error {
	allOK := true
	check := func(name string, pass bool, detail string) {
		status := "ok"
		if !pass {
			status, allOK = "FAIL", false
		}
		fmt.Fprintf(w, "[%s] %s: %s\n", status, name, detail)
	}

	var jm string
	if err := a.Store.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&jm); err != nil {
		return err
	}
	check("wal", jm == "wal", jm)

	// integrity_check returns "ok" as a single row when healthy, or one row per
	// problem when corrupt — read them all so the detail isn't silently truncated.
	rows, err := a.Store.DB.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	var msgs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	integ := strings.Join(msgs, "; ")
	check("integrity", integ == "ok", integ)

	var uv int
	if err := a.Store.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&uv); err != nil {
		return err
	}
	check("schema", uv >= 1, fmt.Sprintf("user_version=%d", uv))

	check("git", a.Git.RepoID != "", fmt.Sprintf("id=%s repo=%s branch=%s nongit=%v",
		a.Git.RepoID, a.Git.RepoName, a.Git.Branch, a.Git.NonGit))

	if !allOK {
		return fmt.Errorf("doctor: one or more checks failed")
	}
	return nil
}
