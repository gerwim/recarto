package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/graph"
	"github.com/gerwim/recarto/internal/memory"
)

func codelessApp(t *testing.T, folder string, repos ...string) *app.App {
	t.Helper()
	clearRecartoEnv(t)
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	t.Setenv("RECARTO_WORKSPACE", "stage-four")
	t.Setenv("RECARTO_REPOS", strings.Join(repos, string(os.PathListSeparator)))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(app.Config{CWD: folder, DecideCodeless: true})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Codeless {
		_ = a.Close()
		t.Fatal("app was not marked as a code-less folder serving repos")
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func indexRepo(t *testing.T, rc *app.RepoContext) {
	t.Helper()
	wt := rc.Git.Worktree
	if err := rc.Graph.ReconcileIfIdle(context.Background(), wt, graph.NewSource(wt), func(at string) string { return graph.WorktreeToken(wt, at) }); err != nil {
		t.Fatal(err)
	}
}

func routeRepos(t *testing.T) (folder, recarto, second string) {
	t.Helper()
	base := t.TempDir()
	folder = filepath.Join(base, "_scratch")
	recarto = filepath.Join(base, "recarto")
	second = filepath.Join(base, "second")
	gitRepoWith(t, recarto, map[string]string{
		"README.md":                  "recarto docs",
		"go.mod":                     "module example.test/recarto\n\ngo 1.24\n",
		"internal/cli/warm.go":       "package cli\nfunc Warm() {}\n",
		"internal/store/filelock.go": "package store\nfunc TryLockFile() {}\n",
		"internal/one/agent.go":      "package one\nfunc Augment() {}\nfunc SessionStart() {}\n",
		"internal/two/agent.go":      "package two\nfunc Augment() {}\nfunc SessionStart() {}\n",
	})
	gitRepoWith(t, second, map[string]string{
		"README.md":                  "second docs",
		"store/filelock.go":          "package store\nfunc TryLockFile() {}\n",
		"internal/store/filelock.go": "package store\nfunc secondOnlyLock() {}\n",
		"internal/session/start.go":  "package session\nfunc SessionStart() {}\n",
	})
	return folder, recarto, second
}

func routeBody(n int) string {
	return fmt.Sprintf("routing case%d cedar%d violet%d marble%d silver%d durable observation", n, n*17+3, n*31+5, n*43+7, n*59+11)
}

func recordRouted(t *testing.T, a *app.App, n int, files, symbols []string) (RecordMemoryOutput, error) {
	t.Helper()
	return recordMemory(context.Background(), a, RecordMemoryInput{
		Kind: "learning", Body: routeBody(n), Files: files, Symbols: symbols,
	})
}

func repoAt(a *app.App, worktree string) *app.RepoContext {
	for _, rc := range a.Repos {
		if rc.Git.Worktree == worktree {
			return rc
		}
	}
	return nil
}

func assertOnlyStored(t *testing.T, a *app.App, id string, want *app.RepoContext) *memory.Record {
	t.Helper()
	var got *memory.Record
	for _, rc := range a.AllRepos() {
		rec, err := rc.Engine.GetByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rc.Engine == want.Engine {
			got = rec
		} else if rec != nil {
			t.Errorf("memory %s also landed in %s", id, rc.Git.Worktree)
		}
	}
	return got
}

func totalMemoryRows(t *testing.T, a *app.App) int {
	t.Helper()
	var total int
	for _, rc := range a.AllRepos() {
		var count int
		if err := rc.Store.DB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		total += count
	}
	return total
}

func recordRawLearning(t *testing.T, eng *memory.Engine, body string) string {
	t.Helper()
	id, _, _, err := eng.Record(context.Background(), memory.NewMemory{Kind: memory.KindLearning, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func getRawRecord(t *testing.T, eng *memory.Engine, id string) *memory.Record {
	t.Helper()
	rec, err := eng.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestRouteWriteTwoIndexedRepos(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])

	cases := []struct {
		name    string
		files   []string
		symbols []string
		want    *app.RepoContext
		refs    []string
	}{
		{name: "unique second file", files: []string{"store/filelock.go"}, want: a.Repos[1], refs: []string{"store/filelock.go"}},
		{name: "absolute second file", files: []string{filepath.Join(second, "store/filelock.go")}, want: a.Repos[1], refs: []string{"store/filelock.go"}},
		{name: "intersection of shared and unique paths", files: []string{"README.md", "store/filelock.go"}, want: a.Repos[1], refs: []string{"README.md", "store/filelock.go"}},
		{name: "unique symbol", symbols: []string{"Warm"}, want: a.Repos[0]},
		{name: "symbol duplicated inside one repo", symbols: []string{"Augment"}, want: a.Repos[0]},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := recordRouted(t, a, i+1, tc.files, tc.symbols)
			if err != nil {
				t.Fatal(err)
			}
			if out.Repo != repoLabel(a, tc.want) {
				t.Fatalf("repo = %q, want %q", out.Repo, repoLabel(a, tc.want))
			}
			rec := assertOnlyStored(t, a, out.ID, tc.want)
			if rec == nil {
				t.Fatal("memory was not stored in the selected repo")
			}
			var refs []string
			for _, ref := range rec.Refs {
				if ref.Type == memory.RefFile {
					refs = append(refs, ref.Value)
				}
			}
			if fmt.Sprint(refs) != fmt.Sprint(tc.refs) {
				t.Fatalf("file refs = %v, want %v", refs, tc.refs)
			}
		})
	}

	refused := []struct {
		name    string
		files   []string
		symbols []string
	}{
		{name: "shared README", files: []string{"README.md"}},
		{name: "shared nested file", files: []string{"internal/store/filelock.go"}},
		{name: "shared symbol", symbols: []string{"TryLockFile"}},
		{name: "shared session symbol", symbols: []string{"SessionStart"}},
		{name: "disjoint file and symbol", files: []string{"store/filelock.go", "internal/cli/warm.go"}},
		{name: "missing file", files: []string{"store/filelock.go", "no/such/file.go"}},
		{name: "no anchors"},
	}
	for i, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			before := totalMemoryRows(t, a)
			_, err := recordRouted(t, a, 100+i, tc.files, tc.symbols)
			if err == nil {
				t.Fatal("write unexpectedly stored")
			}
			for _, want := range []string{"recarto", "second", "scope: \"workspace\""} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			if got := totalMemoryRows(t, a); got != before {
				t.Fatalf("memory rows = %d, want unchanged %d", got, before)
			}
		})
	}

	bug, err := recordBug(context.Background(), a, RecordBugInput{ErrorMessage: "bug file route unique", File: "store/filelock.go"})
	if err != nil || bug.Repo != repoLabel(a, a.Repos[1]) {
		t.Fatalf("recordBug(file) = %+v, %v", bug, err)
	}
	if got := assertOnlyStored(t, a, bug.ID, a.Repos[1]); got == nil || len(got.Refs) != 1 || got.Refs[0].Value != "store/filelock.go" {
		t.Fatalf("recordBug refs = %+v", got)
	}
	_, err = recordBug(context.Background(), a, RecordBugInput{ErrorMessage: "bug no anchors unique"})
	if err == nil || strings.Contains(err.Error(), "workspace") || !strings.Contains(err.Error(), "recarto") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("recordBug without anchors refusal = %v", err)
	}
}

func TestRouteWriteMatchesSymbolsLiterally(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	commitRouteFile(t, second, "Methods.cs", "namespace Ns { class T { void DoXRun() {} void run() {} } }")
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])

	out, err := recordRouted(t, a, 580, nil, []string{"T.run"})
	if err != nil || out.Repo != repoLabel(a, a.Repos[1]) {
		t.Fatalf("symbol T.run = %+v, %v; want second", out, err)
	}
	if rec := assertOnlyStored(t, a, out.ID, a.Repos[1]); rec == nil {
		t.Fatal("T.run memory was not stored in second")
	}

	before := totalMemoryRows(t, a)
	_, err = recordRouted(t, a, 581, nil, []string{"T.Run"})
	if err == nil || !strings.Contains(err.Error(), "- T.Run: no served repo") {
		t.Fatalf("symbol T.Run refusal = %v, want no served repo", err)
	}
	if got := totalMemoryRows(t, a); got != before {
		t.Fatalf("memory rows = %d, want unchanged %d", got, before)
	}
}

func TestRouteWriteSymlinksAndAliases(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])
	link := filepath.Join(recarto, "link-to-second.go")
	if err := os.Symlink(filepath.Join(second, "store/filelock.go"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(outside, []byte("package outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(recarto, "link-outside.go")); err != nil {
		t.Fatal(err)
	}

	for i, files := range [][]string{{"link-to-second.go"}, {"link-to-second.go", filepath.Join(second, "store/filelock.go")}} {
		out, err := recordRouted(t, a, i+201, files, nil)
		if err != nil || out.Repo != "second" {
			t.Fatalf("recordMemory(%v) = %+v, %v", files, out, err)
		}
		rec := assertOnlyStored(t, a, out.ID, a.Repos[1])
		if rec == nil || len(rec.Refs) != 1 || rec.Refs[0].Value != "store/filelock.go" {
			t.Fatalf("stored refs for %v = %+v", files, rec)
		}
	}
	before := totalMemoryRows(t, a)
	_, err := recordRouted(t, a, 203, []string{"link-outside.go"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no served repo") {
		t.Fatalf("outside link refusal = %v", err)
	}
	if got := totalMemoryRows(t, a); got != before {
		t.Fatalf("outside link stored memory: rows %d, want %d", got, before)
	}
}

func TestRouteWriteUnfinishedIndexBlocksOnlySymbols(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])

	for i, tc := range []struct {
		files   []string
		symbols []string
		want    *app.RepoContext
		blocked bool
	}{
		{symbols: []string{"Warm"}, blocked: true},
		{files: []string{"internal/cli/warm.go"}, symbols: []string{"Warm"}, want: a.Repos[0]},
		{symbols: []string{"NoSuchSymbolXyz"}, blocked: true},
		{files: []string{"store/filelock.go"}, symbols: []string{"secondOnlyLock"}, blocked: true},
		{files: []string{"store/filelock.go"}, want: a.Repos[1]},
	} {
		out, err := recordRouted(t, a, 300+i, tc.files, tc.symbols)
		if tc.blocked {
			if err == nil || !strings.Contains(err.Error(), "second (index unfinished)") {
				t.Fatalf("case %+v refusal = %v", tc, err)
			}
			continue
		}
		if err != nil || out.Repo != repoLabel(a, tc.want) {
			t.Fatalf("case %+v = %+v, %v", tc, out, err)
		}
	}
	built, _, err := a.Repos[1].Graph.IndexedSymbols(context.Background(), a.Repos[1].Git.Worktree, false, []string{"Warm"})
	if err != nil || built {
		t.Fatalf("routing started the second repo's index: built=%v err=%v", built, err)
	}

	indexRepo(t, a.Repos[1])
	for i, tc := range []struct {
		files   []string
		symbols []string
		want    *app.RepoContext
		refused bool
	}{
		{symbols: []string{"Warm"}, want: a.Repos[0]},
		{files: []string{"store/filelock.go"}, symbols: []string{"secondOnlyLock"}, want: a.Repos[1]},
		{symbols: []string{"NoSuchSymbolXyz"}, refused: true},
	} {
		out, err := recordRouted(t, a, 320+i, tc.files, tc.symbols)
		if tc.refused {
			if err == nil || strings.Contains(err.Error(), "index unfinished") || !strings.Contains(err.Error(), "recarto") || !strings.Contains(err.Error(), "second") {
				t.Fatalf("missing-symbol refusal = %v", err)
			}
		} else if err != nil || out.Repo != repoLabel(a, tc.want) {
			t.Fatalf("finished-index case = %+v, %v", out, err)
		}
	}
}

func TestRouteWriteColdAndDeletedCode(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "_scratch")
	recarto := filepath.Join(base, "recarto")
	docs := filepath.Join(base, "docs")
	gone := filepath.Join(base, "gone")
	gitRepoWith(t, recarto, map[string]string{
		"README.md":            "recarto docs",
		"internal/cli/warm.go": "package cli\nfunc Warm() {}\n",
	})
	gitRepoWith(t, docs, map[string]string{"README.md": "docs only"})
	gitRepoWith(t, gone, map[string]string{"g.go": "package gone\nfunc GoneFunc() {}\n"})
	a := codelessApp(t, folder, recarto, docs, gone)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[2])
	if err := os.Remove(filepath.Join(gone, "g.go")); err != nil {
		t.Fatal(err)
	}

	out, err := recordRouted(t, a, 401, nil, []string{"Warm"})
	if err != nil || out.Repo != "recarto" {
		t.Fatalf("Warm with a cold docs repo = %+v, %v", out, err)
	}
	_, err = recordRouted(t, a, 402, nil, []string{"GoneFunc"})
	if err == nil || strings.Contains(err.Error(), "index unfinished") || !strings.Contains(err.Error(), "no served repo") {
		t.Fatalf("deleted symbol refusal = %v", err)
	}
}

func TestRouteWriteThreeReposAndNestedOwnership(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "_scratch")
	recarto := filepath.Join(base, "recarto")
	second := filepath.Join(base, "second")
	third := filepath.Join(base, "third")
	gitRepoWith(t, recarto, map[string]string{"README.md": "recarto", "go.mod": "module recarto\n", "internal/warm.go": "package internal\nfunc Warm() {}\n"})
	gitRepoWith(t, second, map[string]string{"README.md": "second"})
	gitRepoWith(t, third, map[string]string{"go.mod": "module third\n"})
	a := codelessApp(t, folder, recarto, second, third)
	indexRepo(t, a.Repos[0])
	for i, files := range [][]string{{"README.md", "go.mod"}, {"go.mod"}} {
		out, err := recordRouted(t, a, 450+i, files, nil)
		if i == 0 {
			if err != nil || out.Repo != "recarto" {
				t.Fatalf("intersection write = %+v, %v", out, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "recarto") || !strings.Contains(err.Error(), "second") || !strings.Contains(err.Error(), "third") {
			t.Fatalf("shared go.mod refusal = %v", err)
		}
	}

	inner := filepath.Join(recarto, "nested", "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRepoWith(t, inner, map[string]string{"x.go": "package inner\nfunc X() {}\n"})
	_ = a.Close()
	a = codelessApp(t, folder, recarto, second, third, inner)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[3])
	innerRC := repoAt(a, inner)
	for i, file := range []string{filepath.Join(inner, "x.go"), "nested/inner/x.go"} {
		out, err := recordRouted(t, a, 470+i, []string{file}, nil)
		if err != nil || out.Repo != repoLabel(a, innerRC) {
			t.Fatalf("nested path %q = %+v, %v", file, out, err)
		}
		rec := assertOnlyStored(t, a, out.ID, innerRC)
		if rec == nil || len(rec.Refs) != 1 || rec.Refs[0].Value != "x.go" {
			t.Fatalf("nested ref = %+v", rec)
		}
	}
	if err := os.MkdirAll(filepath.Join(inner, "nested/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "nested/inner/x.go"), []byte("package inner\nfunc Y() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := recordRouted(t, a, 472, []string{"nested/inner/x.go"}, nil)
	if err == nil || !strings.Contains(err.Error(), "inner: nested/inner/x.go") || !strings.Contains(err.Error(), "inner: x.go") {
		t.Fatalf("ambiguous nested path refusal = %v", err)
	}
	for i, tc := range []struct{ file, rel string }{
		{filepath.Join(inner, "x.go"), "x.go"},
		{filepath.Join(inner, "nested/inner/x.go"), "nested/inner/x.go"},
	} {
		out, err := recordRouted(t, a, 473+i, []string{tc.file}, nil)
		if err != nil || out.Repo != repoLabel(a, innerRC) {
			t.Fatalf("absolute nested path %q = %+v, %v", tc.file, out, err)
		}
		rec := assertOnlyStored(t, a, out.ID, innerRC)
		if rec == nil || len(rec.Refs) != 1 || rec.Refs[0].Value != tc.rel {
			t.Fatalf("absolute nested ref = %+v, want %q", rec, tc.rel)
		}
	}
}

func TestRouteWriteSupersedesOnlyWhereTheOldMemoryLives(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])
	oldID := recordRawLearning(t, a.Repos[0].Engine, "old fact about the route transaction and write ownership")

	out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(501), SupersedesID: oldID})
	if err != nil || out.Repo != "recarto" || out.Superseded != oldID {
		t.Fatalf("supersede by holder = %+v, %v", out, err)
	}
	if old := getRawRecord(t, a.Repos[0].Engine, oldID); old == nil || old.Status != "superseded" || old.SupersededBy != out.ID {
		t.Fatalf("old record after replacement = %+v", old)
	}

	warmID := recordRawLearning(t, a.Repos[0].Engine, "another old fact about transaction isolation")
	out, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(502), Symbols: []string{"Warm"}, SupersedesID: warmID})
	if err != nil || out.Repo != "recarto" || out.Superseded != warmID {
		t.Fatalf("supersede narrowed by symbol = %+v, %v", out, err)
	}

	fileID := recordRawLearning(t, a.Repos[0].Engine, "old fact that must remain in the first repo")
	_, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(503), Files: []string{"store/filelock.go"}, SupersedesID: fileID})
	if err == nil || !strings.Contains(err.Error(), "supersedes_id "+fileID+": recarto") || !strings.Contains(err.Error(), `pass repo as one of: "recarto"`) || !strings.Contains(err.Error(), `in "second"`) {
		t.Fatalf("disjoint replacement refusal = %v", err)
	}
	if old := getRawRecord(t, a.Repos[0].Engine, fileID); old == nil || old.Status != "active" {
		t.Fatalf("refused replacement changed old record: %+v", old)
	}

	_, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(504), SupersedesID: "unknown-memory-id"})
	if err == nil || !strings.Contains(err.Error(), "no served repo's store holds memory unknown-memory-id") {
		t.Fatalf("unknown replacement refusal = %v", err)
	}

	folderID := recordRawLearning(t, a.Engine, "old fact kept in the scratch folder store")
	_, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(505), SupersedesID: folderID})
	if err == nil || !strings.Contains(err.Error(), "this folder's own store") || !strings.Contains(err.Error(), `pass repo: "_scratch"`) {
		t.Fatalf("folder-store replacement refusal = %v", err)
	}

	workspaceID := recordRawLearning(t, a.Workspace.Engine, "old fact kept in the workspace store")
	_, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(506), SupersedesID: workspaceID})
	if err == nil || !strings.Contains(err.Error(), "workspace store") || !strings.Contains(err.Error(), `scope: "workspace"`) {
		t.Fatalf("workspace replacement refusal = %v", err)
	}
}

func TestExplicitRepoWriteDoesNotReplaceMemoryInAnotherStore(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	explicitID := recordRawLearning(t, a.Repos[0].Engine, "old fact for an explicit repo write")
	out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(507), Repo: "second", SupersedesID: explicitID})
	if err != nil || out.Repo != "" {
		t.Fatalf("explicit repo write = %+v, %v", out, err)
	}
	if rec := getRawRecord(t, a.Repos[1].Engine, out.ID); rec == nil {
		t.Fatal("explicit repo write was not stored in second")
	}
	if old := getRawRecord(t, a.Repos[0].Engine, explicitID); old == nil || old.Status != "active" {
		t.Fatalf("explicit repo write replaced a memory in another store: %+v", old)
	}
	var queued int
	if err := a.Repos[1].Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_sync_outbox`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("second outbox count = %d, err %v; want only the new memory", queued, err)
	}
}

func TestRouteWriteReplacementRechecksTheSelectedStore(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])
	ctx := context.Background()

	t.Run("target disappears", func(t *testing.T) {
		oldID := recordRawLearning(t, a.Repos[0].Engine, "old target removed between route and write")
		body := routeBody(520)
		previous := beforeRoutedWrite
		beforeRoutedWrite = func() {
			if _, err := a.Repos[0].Engine.Forget(ctx, oldID, "test"); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { beforeRoutedWrite = previous })
		_, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: body, SupersedesID: oldID})
		if err == nil || !strings.Contains(err.Error(), "no served repo's store holds memory "+oldID) || !strings.Contains(err.Error(), "record again without supersedes_id") {
			t.Fatalf("disappeared-target response = %v", err)
		}
		for _, rc := range a.AllRepos() {
			recs, listErr := rc.Engine.List(ctx, memory.ListFilter{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, rec := range recs {
				if rec.Body == body {
					t.Fatalf("replacement body was stored in %s", rc.Git.Worktree)
				}
			}
		}
	})

	t.Run("target moves to another store", func(t *testing.T) {
		oldID := recordRawLearning(t, a.Repos[0].Engine, "old target copied between route and write")
		old := getRawRecord(t, a.Repos[0].Engine, oldID)
		body := routeBody(521)
		previous := beforeRoutedWrite
		beforeRoutedWrite = func() {
			if _, _, err := a.Repos[1].Engine.SyncApply(ctx, memory.ToWireRecord(old)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Repos[0].Engine.Forget(ctx, oldID, "test"); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { beforeRoutedWrite = previous })
		_, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: body, SupersedesID: oldID})
		for _, want := range []string{fmt.Sprintf("memory %s was not in recarto's store when the write ran; it is now in second.", oldID), `To replace it, pass repo as one of: "second".`, "record again without supersedes_id", `scope: "workspace"`} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("moved-target response %q does not contain %q", err, want)
			}
		}
		if rec := getRawRecord(t, a.Repos[1].Engine, oldID); rec == nil || rec.Status != "active" {
			t.Fatalf("copied target was changed by failed write: %+v", rec)
		}
		for _, rc := range a.AllRepos() {
			recs, listErr := rc.Engine.List(ctx, memory.ListFilter{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, rec := range recs {
				if rec.Body == body {
					t.Fatalf("replacement body was stored in %s", rc.Git.Worktree)
				}
			}
		}
		beforeRoutedWrite = previous
		out, err := recordMemory(ctx, a, RecordMemoryInput{Kind: "learning", Body: routeBody(522), SupersedesID: oldID})
		if err != nil || out.Repo != "second" || out.Superseded != oldID {
			t.Fatalf("retry after target moved = %+v, %v", out, err)
		}
		if rec := getRawRecord(t, a.Repos[1].Engine, oldID); rec == nil || rec.Status != "superseded" {
			t.Fatalf("retry did not replace second's copy: %+v", rec)
		}
	})
}

func TestRouteWriteReplacementAcrossClones(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "_scratch")
	app1 := filepath.Join(base, "app-1")
	app2 := filepath.Join(base, "app-2")
	gitRepoWith(t, app1, map[string]string{"README.md": "shared repo"})
	gitClone(t, app1, app2)
	a := codelessApp(t, folder, app1, app2)
	oldID := recordRawLearning(t, a.Repos[0].Engine, "memory shared by two local stores")
	old := getRawRecord(t, a.Repos[0].Engine, oldID)
	if _, _, err := a.Repos[1].Engine.SyncApply(context.Background(), memory.ToWireRecord(old)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app2, "only-here.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(530), SupersedesID: oldID})
	if err == nil || !strings.Contains(err.Error(), `"app-1", "app-2"`) {
		t.Fatalf("ambiguous clone replacement = %v", err)
	}
	for _, rc := range a.Repos {
		if rec := getRawRecord(t, rc.Engine, oldID); rec == nil || rec.Status != "active" {
			t.Fatalf("clone %s copy changed after refusal: %+v", rc.Git.Worktree, rec)
		}
	}
	out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(531), Files: []string{"only-here.go"}, SupersedesID: oldID})
	if err != nil || out.Repo != "app-2" || out.Superseded != oldID {
		t.Fatalf("file-narrowed clone replacement = %+v, %v", out, err)
	}
	if rec := getRawRecord(t, a.Repos[0].Engine, oldID); rec == nil || rec.Status != "active" {
		t.Fatalf("app-1 copy changed: %+v", rec)
	}
	if rec := getRawRecord(t, a.Repos[1].Engine, oldID); rec == nil || rec.Status != "superseded" {
		t.Fatalf("app-2 copy not replaced: %+v", rec)
	}
	out, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(532), Repo: "app-1", SupersedesID: oldID})
	if err != nil || getRawRecord(t, a.Repos[0].Engine, oldID).Status != "superseded" {
		t.Fatalf("explicit clone replacement = %+v, %v", out, err)
	}
}

func TestRepoSelectorsNameCollidingReposInReplies(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "_scratch")
	app1, app2 := filepath.Join(base, "a", "app"), filepath.Join(base, "b", "app")
	gitRepoWith(t, app1, map[string]string{"README.md": "first app"})
	gitRepoWith(t, app2, map[string]string{"README.md": "second app"})
	a := codelessApp(t, folder, app1, app2)
	_, err := recordRouted(t, a, 540, []string{"README.md"}, nil)
	if err == nil || !strings.Contains(err.Error(), app1) || !strings.Contains(err.Error(), app2) || strings.Contains(err.Error(), `"app"`) {
		t.Fatalf("same-name refusal = %v", err)
	}
	out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(541), Repo: app2})
	if err != nil || out.Repo != "" {
		t.Fatalf("path-selected write = %+v, %v", out, err)
	}
	if rec := assertOnlyStored(t, a, out.ID, a.Repos[1]); rec == nil {
		t.Fatal("path-selected write did not reach second clone")
	}
	listed, err := listMemory(context.Background(), a, ListMemoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rec := range listed.Memories {
		if rec.ID == out.ID {
			found = true
			if rec.Repo != app2 {
				t.Errorf("list_memory repo = %q, want selector %q", rec.Repo, app2)
			}
		}
	}
	if !found {
		t.Fatal("list_memory omitted the stored record")
	}
	got, err := getMemory(context.Background(), a, GetMemoryInput{ID: out.ID})
	if err != nil || !got.Found || got.Memory.Repo != app2 {
		t.Fatalf("get_memory = %+v, %v; want selector %q", got, err, app2)
	}
}

func TestCodeLessFolderNameCollisionAndRepoErrors(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "second")
	recarto, second := filepath.Join(base, "recarto"), filepath.Join(base, "repos", "second")
	gitRepoWith(t, recarto, map[string]string{"internal/store/filelock.go": "package store\nfunc TryLockFile() {}\n"})
	gitRepoWith(t, second, map[string]string{"store/filelock.go": "package store\nfunc TryLockFile() {}\n"})
	a := codelessApp(t, folder, recarto, second)
	indexRepo(t, a.Repos[0])
	indexRepo(t, a.Repos[1])

	_, err := recordRouted(t, a, 550, nil, []string{"TryLockFile"})
	if err == nil || !strings.Contains(err.Error(), "recarto") || !strings.Contains(err.Error(), second) || strings.Contains(err.Error(), `"second"`) {
		t.Fatalf("folder/repo name collision refusal = %v", err)
	}
	out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(551), Repo: second})
	if err != nil {
		t.Fatalf("path retry: %v", err)
	}
	if rec := assertOnlyStored(t, a, out.ID, a.Repos[1]); rec == nil {
		t.Fatal("selector path stored into the folder or wrong repo")
	}
	listed, err := listMemory(context.Background(), a, ListMemoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range listed.Memories {
		if rec.ID == out.ID && rec.Repo != second {
			t.Fatalf("listed repo = %q, want %q", rec.Repo, second)
		}
	}
	_, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(552), Repo: "nosuch"})
	if err == nil || !strings.HasSuffix(err.Error(), "serving: recarto, "+second) {
		t.Fatalf("unknown repo response = %v", err)
	}

	folderWrite, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(553), Repo: "second"})
	if err != nil || getRawRecord(t, a.Engine, folderWrite.ID) == nil {
		t.Fatalf("repo name resolving to folder = %+v, %v", folderWrite, err)
	}
	if rec, _ := a.Repos[1].Engine.GetByID(context.Background(), folderWrite.ID); rec != nil {
		t.Fatal("repo name unexpectedly reached the colliding served repo")
	}
}

func TestCodeLessErrorsOmitFolderAndNoServedRepoKeepsFolderWrites(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	_, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(560), Repo: "nosuch"})
	if err == nil || !strings.HasSuffix(err.Error(), "serving: recarto, second") || strings.Contains(err.Error(), folder) {
		t.Fatalf("unknown repo response = %v", err)
	}
	_, err = searchGraph(context.Background(), a, SearchGraphInput{Query: "nope", Repo: "nosuch"})
	if err == nil || !strings.HasSuffix(err.Error(), "serving: recarto, second") || strings.Contains(err.Error(), folder) {
		t.Fatalf("search_graph unknown repo response = %v", err)
	}
	folderWrite, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(561), Repo: filepath.Base(folder)})
	if err != nil || getRawRecord(t, a.Engine, folderWrite.ID) == nil {
		t.Fatalf("explicit folder repo write = %+v, %v", folderWrite, err)
	}

	t.Run("no repo served", func(t *testing.T) {
		clearRecartoEnv(t)
		t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
		plain := t.TempDir()
		appWithoutRepos, err := app.Open(app.Config{CWD: plain, DecideCodeless: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := appWithoutRepos.Close(); err != nil {
				t.Errorf("close app: %v", err)
			}
		})
		out, err := recordMemory(context.Background(), appWithoutRepos, RecordMemoryInput{Kind: "learning", Body: routeBody(562)})
		if err != nil || out.Repo != "" || getRawRecord(t, appWithoutRepos.Engine, out.ID) == nil {
			t.Fatalf("folder-local write = %+v, %v", out, err)
		}
		recalled, err := recallMemory(context.Background(), appWithoutRepos, RecallMemoryInput{TaskHint: "routing case562"})
		if err != nil || recalled.Count == 0 {
			t.Fatalf("folder recall = %+v, %v", recalled, err)
		}
	})
}

func TestCodeLessWorkspaceAndExplicitRepoWritesStayExplicit(t *testing.T) {
	folder, recarto, second := routeRepos(t)
	a := codelessApp(t, folder, recarto, second)
	workspaceOut, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(565), Scope: scopeWorkspace})
	if err != nil || workspaceOut.Repo != "" || getRawRecord(t, a.Workspace.Engine, workspaceOut.ID) == nil {
		t.Fatalf("workspace write = %+v, %v", workspaceOut, err)
	}
	explicitOut, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(566), Repo: "second"})
	if err != nil || explicitOut.Repo != "" || getRawRecord(t, a.Repos[1].Engine, explicitOut.ID) == nil {
		t.Fatalf("explicit repo write = %+v, %v", explicitOut, err)
	}
	if _, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(567), Scope: "invalid"}); err == nil || !strings.Contains(err.Error(), "invalid scope") {
		t.Fatalf("invalid scope = %v", err)
	}
}

func TestCodeLessWriteRoutesOneServedRepo(t *testing.T) {
	folder, recarto, _ := routeRepos(t)
	a := codelessApp(t, folder, recarto)
	out, err := recordRouted(t, a, 568, nil, nil)
	if err != nil || out.Repo != "recarto" || assertOnlyStored(t, a, out.ID, a.Repos[0]) == nil {
		t.Fatalf("one-repo write = %+v, %v", out, err)
	}
	oldID := recordRawLearning(t, a.Repos[0].Engine, "single served repo old fact to replace")
	out, err = recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(569), SupersedesID: oldID})
	if err != nil || out.Repo != "recarto" || out.Superseded != oldID || getRawRecord(t, a.Repos[0].Engine, oldID).Status != "superseded" {
		t.Fatalf("one-repo replacement = %+v, %v", out, err)
	}
}

func TestGitSessionWritesRemainInThePrimaryRepo(t *testing.T) {
	for i, serveExtra := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra-%v", serveExtra), func(t *testing.T) {
			clearRecartoEnv(t)
			_, recarto, second := routeRepos(t)
			t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
			if serveExtra {
				t.Setenv("RECARTO_REPOS", second)
			}
			a, err := app.Open(app.Config{CWD: recarto, DecideCodeless: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := a.Close(); err != nil {
					t.Errorf("close app: %v", err)
				}
			})
			if a.Codeless {
				t.Fatal("git session was marked as code-less")
			}
			out, err := recordMemory(context.Background(), a, RecordMemoryInput{Kind: "learning", Body: routeBody(590 + i)})
			if err != nil || out.Repo != "" || getRawRecord(t, a.Engine, out.ID) == nil {
				t.Fatalf("git-session write = %+v, %v", out, err)
			}
			if serveExtra && getRawRecord(t, a.Repos[0].Engine, out.ID) != nil {
				t.Fatal("git-session write was routed to the extra repo")
			}
		})
	}
}

func gitClone(t *testing.T, source, target string) {
	t.Helper()
	cmd := exec.Command("git", "clone", "-q", source, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
}

func TestUnselectableRepoIsNamedButNeverOffered(t *testing.T) {
	base := t.TempDir()
	folder := filepath.Join(base, "_scratch")
	first, second := filepath.Join(base, "RECARTO"), filepath.Join(base, "recarto")
	gitRepoWith(t, first, map[string]string{"README.md": "first", "first.go": "package first\n"})
	gitRepoWith(t, second, map[string]string{"README.md": "second", "only-second.go": "package second\n"})
	if _, err := os.Stat(filepath.Join(base, "ReCaRtO")); err == nil {
		t.Skip("case-insensitive file system")
	}
	a := codelessApp(t, folder, first, second)
	if _, ok := a.Selector(a.Repos[1]); ok {
		t.Fatal("later case-colliding repo unexpectedly has a selector")
	}

	_, err := recordRouted(t, a, 570, []string{"README.md"}, nil)
	mark := filepath.Join(base, "recarto") + " (" + app.Unselectable + ")"
	if err == nil || !strings.Contains(err.Error(), mark) || strings.Contains(err.Error(), fmt.Sprintf("%q", second)) {
		t.Fatalf("unselectable repo refusal = %v", err)
	}
	out, err := recordRouted(t, a, 571, []string{"only-second.go"}, nil)
	if err != nil || out.Repo != mark {
		t.Fatalf("unselectable repo success = %+v, %v", out, err)
	}
}

type cancelRouteRefresh struct{ cancel context.CancelFunc }

func (p cancelRouteRefresh) Start(int) {}
func (p cancelRouteRefresh) Inc()      {}
func (p cancelRouteRefresh) Done()     {}
func (p cancelRouteRefresh) Phase(label string) {
	if strings.HasPrefix(label, "resolving references") {
		p.cancel()
	}
}

func cutRouteRefresh(t *testing.T, rc *app.RepoContext) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	err := rc.Graph.ReconcileIfStale(ctx, rc.Git.Worktree, graph.NewSource(rc.Git.Worktree), graph.WorktreeToken(rc.Git.Worktree, ""), graph.WithProgress(cancelRouteRefresh{cancel}))
	cancel()
	if err == nil {
		t.Fatal("refresh was not interrupted")
	}
}

func appendRouteCode(t *testing.T, repo, path, src string) {
	t.Helper()
	p := filepath.Join(repo, filepath.FromSlash(path))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(src); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func commitRouteFile(t *testing.T, repo, path, src string) {
	t.Helper()
	p := filepath.Join(repo, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-q", "-m", "fixture update")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func TestRouteWriteUsesIndexAsItStandsDuringRefresh(t *testing.T) {
	t.Run("refreshed file has no symbol", func(t *testing.T) {
		folder, recarto, second := routeRepos(t)
		a := codelessApp(t, folder, recarto, second)
		indexRepo(t, a.Repos[0])
		indexRepo(t, a.Repos[1])
		appendRouteCode(t, second, "store/filelock.go", "\nfunc AddedDuringRefresh() {}\n")
		cutRouteRefresh(t, a.Repos[1])
		out, err := recordRouted(t, a, 580, nil, []string{"Warm"})
		if err != nil || out.Repo != "recarto" || strings.Contains(fmt.Sprint(err), "index unfinished") {
			t.Fatalf("write during refresh = %+v, %v", out, err)
		}
	})

	t.Run("untouched file still has symbol", func(t *testing.T) {
		folder, recarto, second := routeRepos(t)
		commitRouteFile(t, second, "warm.go", "package second\nfunc Warm() {}\n")
		a := codelessApp(t, folder, recarto, second)
		indexRepo(t, a.Repos[0])
		indexRepo(t, a.Repos[1])
		appendRouteCode(t, second, "store/filelock.go", "\nfunc AddedDuringRefresh() {}\n")
		cutRouteRefresh(t, a.Repos[1])
		before := totalMemoryRows(t, a)
		_, err := recordRouted(t, a, 581, nil, []string{"Warm"})
		if err == nil || !strings.Contains(err.Error(), "recarto") || !strings.Contains(err.Error(), "second") || strings.Contains(err.Error(), "index unfinished") {
			t.Fatalf("ambiguous refreshed index refusal = %v", err)
		}
		if got := totalMemoryRows(t, a); got != before {
			t.Fatalf("refresh refusal stored a memory: rows %d, want %d", got, before)
		}
	})
}
