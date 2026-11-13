package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gerwim/recarto/internal/update"
	"github.com/gerwim/recarto/internal/version"
)

// ErrUpdateAvailable is returned by `recarto update --check` when a newer release
// exists. main.go maps it to a distinct exit code (10) so scripts can detect it.
var ErrUpdateAvailable = errors.New("update available")

// Update implements `recarto update`. stdin/isTTY are injected for testability;
// main.go passes os.Stdin and the real terminal check.
func Update(out io.Writer, stdin io.Reader, isTTY bool, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)
	wantVersion := fs.String("version", "", "install a specific version (downgrade/pin)")
	check := fs.Bool("check", false, "report whether an update is available; make no changes")
	list := fs.Bool("list", false, "list available versions; make no changes")
	yes := fs.Bool("yes", false, "skip confirmation prompts")
	fs.BoolVar(yes, "y", false, "skip confirmation prompts (shorthand)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	u := &update.Updater{
		Current:  version.Version,
		Platform: update.CurrentPlatform(),
		PubKey:   update.ManifestPublicKey,
		Fetcher:  update.NewHTTPFetcher(),
		Applier:  update.SelfUpdateApplier{}, // empty TargetPath => running binary
	}
	ctx := context.Background()

	st, err := u.Check(ctx)
	if err != nil {
		return err
	}

	if *list {
		printVersions(out, st)
		return nil
	}
	if *check {
		if st.UpToDate {
			fmt.Fprintf(out, "recarto %s is up to date.\n", st.Current)
			return nil
		}
		fmt.Fprintf(out, "update available: %s -> %s\n", st.Current, st.Latest)
		return ErrUpdateAvailable
	}

	target, proceed, err := selectTarget(st.Manifest, st.Current, *wantVersion, isTTY && !*yes, stdin, out)
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintln(out, "no changes made.")
		return nil
	}
	// Skip a redundant re-install when the auto/Enter-selected target equals the
	// current release (e.g. `update --yes` on the latest). An explicit --version
	// is honored as a deliberate reinstall.
	if *wantVersion == "" && update.IsReleaseBuild(st.Current) && update.CompareVersions(target, st.Current) == 0 {
		fmt.Fprintf(out, "recarto %s is already up to date.\n", st.Current)
		return nil
	}
	if !update.IsReleaseBuild(st.Current) {
		fmt.Fprintf(out, "note: current build %q is not a release build.\n", st.Current)
	}
	fmt.Fprintf(out, "installing recarto %s ...\n", target)
	if err := u.InstallVersion(ctx, target); err != nil {
		return updateApplyHint(err)
	}
	fmt.Fprintf(out, "recarto is now %s.\n", target)
	return nil
}

// selectTarget decides which version to install. An explicit flagVersion wins (and
// must exist). Otherwise non-interactive picks Latest; interactive shows a numbered
// menu (newest-first, Enter = latest, q = cancel).
func selectTarget(m *update.Manifest, current, flagVersion string, interactive bool, stdin io.Reader, out io.Writer) (string, bool, error) {
	versions := m.SortedVersions()
	if flagVersion != "" {
		if _, ok := m.Find(flagVersion); !ok {
			return "", false, fmt.Errorf("version %s is not available (try: recarto update --list)", flagVersion)
		}
		return flagVersion, true, nil
	}
	if !interactive {
		return m.Latest, true, nil
	}
	fmt.Fprintf(out, "current: %s   latest: %s\n", current, m.Latest)
	fmt.Fprintln(out, "select a version to install (Enter = latest, q = cancel):")
	for i, r := range versions {
		marker := ""
		if r.Version == m.Latest {
			marker = "  (latest)"
		}
		if r.Version == current {
			marker += "  (current)"
		}
		fmt.Fprintf(out, "  %d) %s%s\n", i+1, r.Version, marker)
	}
	if _, err := fmt.Fprint(out, "> "); err != nil {
		return "", false, err
	}
	raw, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", false, fmt.Errorf("read selection: %w", err)
	}
	line := strings.TrimSpace(raw)
	switch {
	case line == "" && err == io.EOF:
		// Ctrl-D / closed pipe with no input: cancel rather than silently install.
		return "", false, nil
	case line == "":
		return m.Latest, true, nil // bare Enter
	case line == "q" || line == "Q":
		return "", false, nil
	}
	n, convErr := strconv.Atoi(line)
	if convErr != nil || n < 1 || n > len(versions) {
		return "", false, fmt.Errorf("invalid selection %q", line)
	}
	return versions[n-1].Version, true, nil
}

func printVersions(out io.Writer, st *update.Status) {
	fmt.Fprintf(out, "current: %s   latest: %s\n", st.Current, st.Latest)
	for _, r := range st.Manifest.SortedVersions() {
		marker := ""
		if r.Version == st.Latest {
			marker = "  (latest)"
		}
		if r.Version == st.Current {
			marker += "  (current)"
		}
		fmt.Fprintf(out, "  %s%s\n", r.Version, marker)
	}
}

// updateApplyHint adds an actionable hint when the swap likely failed due to file
// permissions on the installed binary.
func updateApplyHint(err error) error {
	if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
		return fmt.Errorf("%w\nhint: the recarto binary is not writable by this user; re-run with elevated privileges or reinstall", err)
	}
	return err
}
