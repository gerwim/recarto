package cli

import (
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gerwim/recarto/internal/cache"
	"github.com/gerwim/recarto/internal/clock"
	"github.com/gerwim/recarto/internal/fullexport"
	"github.com/gerwim/recarto/internal/gitctx"
	"github.com/gerwim/recarto/internal/remotesync"
)

// importHTTPTimeout is generous relative to sync.go's per-request timeouts
// (RECARTO_SYNC_TIMEOUT, default 5s): a bulk multi-project archive can be far
// larger than any single incremental push/pull.
const importHTTPTimeout = 5 * time.Minute

// Export writes (recarto export) — and optionally uploads (--push) — a
// recarto-export/v1 archive covering EVERY per-repo store under the cache
// dir (every status, including archived/done/superseded — not just the
// active rows ordinary recall surfaces — see internal/fullexport), plus the
// global feedback store. This is the one-shot bulk migration path for a
// machine's pre-existing history: ordinary incremental HTTP sync
// (`recarto sync`) only ever ships a store's outbox — writes made after sync
// was configured — so it can never move memories recorded before that.
//
// It never touches the code graph: internal/fullexport reads only the
// portable memory/tombstone tables, exactly like the ordinary sync wire.
func Export(ctx context.Context, w io.Writer, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "output file (default: recarto-export-<date>.ndjson.gz; \"-\" for stdout)")
	repoOnly := fs.Bool("repo", false, "restrict the export to the current directory's repo only")
	push := fs.Bool("push", false, "upload the archive to the sync server after writing it")
	url := fs.String("url", "", "sync server URL (default: $RECARTO_SYNC_URL)")
	key := fs.String("key", "", "sync server key (default: $RECARTO_SYNC_KEY)")
	if helped, err := parseFlags(fs, args); helped || err != nil {
		return err
	}

	outPath := *out
	if outPath == "" {
		outPath = "recarto-export-" + time.Now().Format("20060102") + ".ndjson.gz"
	}
	if *push && outPath == "-" {
		return fmt.Errorf("recarto export: --push cannot be combined with --out -; write to a file first, then `recarto import` it")
	}

	var only, onlyKey string
	if *repoOnly {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		gc, err := gitctx.Resolve(gitctx.Real{Dir: cwd}, cwd)
		if err != nil {
			return err
		}
		only = gc.RepoID
		// Resolve the key live, the same way `recarto sync` does (honoring
		// RECARTO_SYNC_REPO) — a store the whole-cache-dir sweep would have to
		// skip for lacking a cached root commit can still be scoped to and
		// exported directly, by setting the override in this shell.
		if key, ok := remotesync.RepoKey(gc); ok {
			onlyKey = key
		}
	}

	cacheDir, err := cache.Dir()
	if err != nil {
		return err
	}

	var f *os.File
	if outPath == "-" {
		f = os.Stdout
	} else {
		f, err = os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
	}

	gw := gzip.NewWriter(f)
	stats, err := fullexport.ExportAll(ctx, cacheDir, clock.Real{}, gw, fullexport.Options{Only: only, OnlyKey: onlyKey})
	if err != nil {
		gw.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	// When the archive itself goes to stdout, human-readable status must go to
	// stderr instead — writing both to stdout would corrupt the archive.
	logw := w
	if outPath == "-" {
		logw = os.Stderr
	}
	fmt.Fprintf(logw, "exported %d repos, %d records, %d tombstones to %s\n",
		stats.Repos, stats.Records, stats.Tombstones, outPath)
	if stats.Skipped > 0 {
		fmt.Fprintf(logw, "skipped %d repo(s) with no resolvable server key (shallow clone / no root commit; "+
			"cd into each and run `RECARTO_SYNC_REPO=<key> recarto export --repo --push` to export it on its own "+
			"— a whole-machine export can't apply one override across many unrelated projects): %s\n",
			stats.Skipped, strings.Join(stats.SkippedRepos, ", "))
	}

	if *push {
		return uploadArchive(ctx, w, outPath, *url, *key)
	}
	return nil
}

// uploadArchive re-opens the archive written to path and streams it to the
// sync server's bulk import endpoint (internal/remotesync.Client.Import).
func uploadArchive(ctx context.Context, w io.Writer, path, urlFlag, keyFlag string) error {
	client, err := importClient(urlFlag, keyFlag)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	resp, err := client.Import(ctx, f)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	fmt.Fprintf(w, "push: %d/%d records accepted across %d repo(s)\n",
		resp.TotalAccepted, resp.TotalEntries, len(resp.Repos))
	return nil
}

// importClient resolves a remotesync.Client from explicit flags, falling
// back to RECARTO_SYNC_URL/RECARTO_SYNC_KEY (the same env vars `recarto sync`
// uses) — so `recarto export --push` needs no extra configuration on a
// machine that already has ordinary HTTP sync set up.
func importClient(urlFlag, keyFlag string) (*remotesync.Client, error) {
	u := urlFlag
	if u == "" {
		u = os.Getenv("RECARTO_SYNC_URL")
	}
	k := keyFlag
	if k == "" {
		k = os.Getenv("RECARTO_SYNC_KEY")
	}
	if u == "" || k == "" {
		return nil, fmt.Errorf("RECARTO_SYNC_URL and RECARTO_SYNC_KEY must be set (or pass --url/--key)")
	}
	return &remotesync.Client{BaseURL: u, Key: k, HTTP: &http.Client{Timeout: importHTTPTimeout}}, nil
}
