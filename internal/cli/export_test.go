package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
	"git.gerwim.nl/gerwim/recarto/internal/gitctx"
	"git.gerwim.nl/gerwim/recarto/internal/ledger"
	"git.gerwim.nl/gerwim/recarto/internal/memory"
	"git.gerwim.nl/gerwim/recarto/internal/store"
	"git.gerwim.nl/gerwim/recarto/internal/syncwire"
)

// seedCacheStore opens (creating) a store at <cacheDir>/<dirName>/store.db,
// writes repo_name/root_commit meta (mirroring app.writeMeta), records one
// memory, and closes it — mimicking a machine's pre-existing per-repo store.
func seedCacheStore(t *testing.T, cacheDir, dirName, rootCommit, body string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(cacheDir, dirName, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.WriteTx(ctx, func(c store.Conn) error {
		for _, kv := range [][2]string{{"repo_name", dirName}, {"root_commit", rootCommit}} {
			if _, err := c.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	eng := memory.New(st, ledger.New(st), clock.Real{})
	if _, _, _, err := eng.Record(ctx, memory.NewMemory{Kind: memory.KindLearning, Title: "t", Body: body}); err != nil {
		t.Fatal(err)
	}
}

// readArchiveEntries gunzips path and decodes every non-header NDJSON line.
func readArchiveEntries(t *testing.T, path string) []syncwire.ExportEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gzr.Close()
	raw, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatal(err)
	}
	var entries []syncwire.ExportEntry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Format string `json:"format"`
		}
		if err := json.Unmarshal(line, &probe); err == nil && probe.Format != "" {
			continue
		}
		var e syncwire.ExportEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestExportWritesGzipArchiveForEveryProject(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	seedCacheStore(t, cacheDir, "dirA", "rootA", "learning from repo A")
	seedCacheStore(t, cacheDir, "dirB", "rootB", "learning from repo B")

	outPath := filepath.Join(t.TempDir(), "export.ndjson.gz")
	var out bytes.Buffer
	if err := Export(context.Background(), &out, []string{"--out", outPath}); err != nil {
		t.Fatal(err)
	}
	if !contains(out.String(), "2 repos") || !contains(out.String(), "2 records") {
		t.Fatalf("summary missing expected counts:\n%s", out.String())
	}

	entries := readArchiveEntries(t, outPath)
	if len(entries) != 2 {
		t.Fatalf("archive entries = %d, want 2", len(entries))
	}
}

func TestExportRepoFlagScopesToCurrentRepo(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	gc, err := gitctx.Resolve(gitctx.Real{Dir: cwd}, cwd)
	if err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	seedCacheStore(t, cacheDir, gc.RepoID, "root-current-repo", "this repo's learning")
	seedCacheStore(t, cacheDir, "some-other-repo-id", "root-other-repo", "a different repo's learning")

	outPath := filepath.Join(t.TempDir(), "export.ndjson.gz")
	var out bytes.Buffer
	if err := Export(context.Background(), &out, []string{"--out", outPath, "--repo"}); err != nil {
		t.Fatal(err)
	}

	entries := readArchiveEntries(t, outPath)
	if len(entries) != 1 || entries[0].Record.Body != "this repo's learning" {
		t.Fatalf("--repo should scope to only the current repo's store, got %+v", entries)
	}
}

func TestExportSkipsRepoWithNoRootCommit(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	seedCacheStore(t, cacheDir, "dirA", "rootA", "has a root commit")
	seedCacheStore(t, cacheDir, "dirShallow", "", "no root commit")

	outPath := filepath.Join(t.TempDir(), "export.ndjson.gz")
	var out bytes.Buffer
	if err := Export(context.Background(), &out, []string{"--out", outPath}); err != nil {
		t.Fatal(err)
	}
	if !contains(out.String(), "skipped 1 repo") || !contains(out.String(), "dirShallow") {
		t.Fatalf("summary should report the skipped repo by name:\n%s", out.String())
	}
	entries := readArchiveEntries(t, outPath)
	if len(entries) != 1 {
		t.Fatalf("archive entries = %d, want 1 (the shallow repo must be excluded)", len(entries))
	}
}

func TestExportPushWithStdoutOutputIsRejected(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)

	var out bytes.Buffer
	err := Export(context.Background(), &out, []string{"--out", "-", "--push"})
	if err == nil {
		t.Fatal("expected --push combined with --out - to be rejected")
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
