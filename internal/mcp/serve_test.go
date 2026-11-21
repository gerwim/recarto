package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gerwim/recarto/internal/app"
	"github.com/gerwim/recarto/internal/clock"
)

// serveCfg is the App config of the serve tests: the fake repo of openAppIn,
// with its store in cacheDir.
func serveCfg(t *testing.T, cacheDir string, quick bool) app.Config {
	t.Helper()
	t.Setenv("RECARTO_CACHE_DIR", cacheDir)
	return app.Config{
		CWD:        "/repo",
		Runner:     fakeGit{branch: "main"},
		Clock:      clock.NewFake(time.UnixMilli(1_700_000_000_000)),
		QuickCheck: quick,
	}
}

func setOpenWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := openWait
	openWait = d
	t.Cleanup(func() { openWait = prev })
}

// startServe runs serve over in-memory transports and connects a client. The
// returned wait closes the client and returns serve's error.
func startServe(t *testing.T, cfg app.Config, open func(context.Context) (*app.App, error)) (*sdk.ClientSession, func() error) {
	t.Helper()
	sv, err := app.Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ct, st := sdk.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, sv, open, st) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatalf("initialize: %v", err)
	}
	waited := false
	wait := func() error {
		waited = true
		_ = cs.Close()
		select {
		case err := <-done:
			cancel()
			return err
		case <-time.After(time.Minute):
			cancel()
			t.Fatal("serve did not return after the session closed")
			return nil
		}
	}
	t.Cleanup(func() {
		if !waited {
			_ = wait()
		}
	})
	return cs, wait
}

func realOpen(cfg app.Config) func(context.Context) (*app.App, error) {
	return func(ctx context.Context) (*app.App, error) { return app.OpenContext(ctx, cfg) }
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func wantServeClosed(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("serve: %v", err)
	}
}

// toolDefs maps each listed tool to its description and input schema as JSON.
func toolDefs(t *testing.T, tools []*sdk.Tool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tl := range tools {
		j, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		out[tl.Name] = tl.Description + "\n" + string(j)
	}
	return out
}

func TestServeAnswersHandshakeBeforeTheStoreOpens(t *testing.T) {
	clearRecartoEnv(t)
	cfg := serveCfg(t, t.TempDir(), false)
	setOpenWait(t, 0)

	opened, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*sdk.Tool{}
	for name, tl := range listTools(t, opened) {
		want[name] = tl
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	cs, wait := startServe(t, cfg, func(ctx context.Context) (*app.App, error) {
		<-release
		return app.OpenContext(ctx, cfg)
	})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list while the store opens: %v", err)
	}
	var wantList []*sdk.Tool
	for _, tl := range want {
		wantList = append(wantList, tl)
	}
	got, exp := toolDefs(t, res.Tools), toolDefs(t, wantList)
	if len(got) != len(exp) {
		t.Errorf("tools/list offers %d tools before the open, want %d", len(got), len(exp))
	}
	for name, def := range exp {
		if got[name] != def {
			t.Errorf("tool %s before the open:\n%s\nwant:\n%s", name, got[name], def)
		}
	}

	r := callTool(t, cs, "recall_memory", map[string]any{"task_hint": "anything"})
	if !r.IsError || !strings.Contains(resultText(r), "still opening its store") {
		t.Errorf("recall_memory while the store opens = %s, want the still-opening error", resultText(r))
	}

	close(release)
	setOpenWait(t, time.Minute)
	if r := callTool(t, cs, "record_memory", map[string]any{"kind": "learning", "body": "served after a background open"}); r.IsError {
		t.Fatalf("record_memory: %s", resultText(r))
	}
	r = callTool(t, cs, "recall_memory", map[string]any{"task_hint": "background open"})
	if r.IsError || !strings.Contains(resultText(r), "served after a background open") {
		t.Errorf("recall_memory after the open = %s, want the recorded memory", resultText(r))
	}
	wantServeClosed(t, wait())
}

func TestServeKeepsCodelessDecisionFromOpen(t *testing.T) {
	clearRecartoEnv(t)
	base := t.TempDir()
	repo := filepath.Join(base, "served")
	gitRepoWith(t, repo, map[string]string{"main.go": "package main\nfunc Main() {}\n"})
	t.Setenv("RECARTO_REPOS", repo)
	t.Setenv("RECARTO_WORKSPACE", "")

	codeLessFolder := filepath.Join(base, "empty")
	if err := os.MkdirAll(codeLessFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{CWD: codeLessFolder, DecideCodeless: true}
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	cs, wait := startServe(t, cfg, realOpen(cfg))
	if r := callTool(t, cs, "list_memory", map[string]any{}); r.IsError {
		t.Fatalf("list_memory: %s", resultText(r))
	}
	if err := os.WriteFile(filepath.Join(codeLessFolder, "x.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := callTool(t, cs, "record_memory", map[string]any{"kind": "learning", "body": "write after code appeared in started folder"})
	if r.IsError || !strings.Contains(resultText(r), `"repo":"served"`) {
		t.Fatalf("write after code appeared = %s, want the served repo label", resultText(r))
	}
	wantServeClosed(t, wait())

	withCode := filepath.Join(base, "with-code")
	if err := os.MkdirAll(withCode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withCode, "x.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECARTO_CACHE_DIR", t.TempDir())
	cfg = app.Config{CWD: withCode, DecideCodeless: true}
	cs, wait = startServe(t, cfg, realOpen(cfg))
	if r := callTool(t, cs, "list_memory", map[string]any{}); r.IsError {
		t.Fatalf("list_memory: %s", resultText(r))
	}
	if err := os.Remove(filepath.Join(withCode, "x.go")); err != nil {
		t.Fatal(err)
	}
	r = callTool(t, cs, "record_memory", map[string]any{"kind": "learning", "body": "write after code disappeared from started folder"})
	if r.IsError || strings.Contains(resultText(r), `"repo"`) {
		t.Fatalf("write after code disappeared = %s, want a folder-local reply", resultText(r))
	}
	wantServeClosed(t, wait())
}

func TestServeHandshakeIgnoresTheStoreWriteLock(t *testing.T) {
	clearRecartoEnv(t)
	t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
	cfg := serveCfg(t, t.TempDir(), true)
	a, err := app.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := a.Store.Path()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	// A different version makes the background open write its meta.
	if _, err := conn.ExecContext(ctx, `UPDATE meta SET value='0.0.0-old' WHERE key='recarto_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	cs, wait := startServe(t, cfg, realOpen(cfg))
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("tools/list while another process holds the write lock: %v", err)
	}

	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	locked = false
	setOpenWait(t, time.Minute)
	if r := callTool(t, cs, "recall_memory", map[string]any{"task_hint": "anything"}); r.IsError {
		t.Errorf("recall_memory once the lock is released: %s", resultText(r))
	}
	wantServeClosed(t, wait())
}

func TestServeCancelsTheOpenWhenTheSessionCloses(t *testing.T) {
	clearRecartoEnv(t)
	cfg := serveCfg(t, t.TempDir(), true)
	started := make(chan struct{})
	_, wait := startServe(t, cfg, func(ctx context.Context) (*app.App, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	wantServeClosed(t, wait())
}

func TestServeRepairsADamagedStoreBeforeTheFirstToolCall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		autoRepair string
	}{{"auto-repair on", ""}, {"auto-repair off", "0"}} {
		t.Run(tc.name, func(t *testing.T) {
			clearRecartoEnv(t)
			t.Setenv("RECARTO_AUTO_REPAIR", "")
			t.Setenv("RECARTO_QUICKCHECK_INTERVAL", "")
			cacheDir := t.TempDir()
			buildCorruptWritePathStore(t, cacheDir)
			t.Setenv("RECARTO_AUTO_REPAIR", tc.autoRepair)
			setOpenWait(t, time.Minute)

			cfg := serveCfg(t, cacheDir, true)
			cs, wait := startServe(t, cfg, realOpen(cfg))
			if _, err := cs.ListTools(context.Background(), nil); err != nil {
				t.Fatalf("tools/list on a damaged store: %v", err)
			}
			r := callTool(t, cs, "recall_memory", map[string]any{"task_hint": "recorded before the corruption"})
			text := resultText(r)
			if tc.autoRepair == "" {
				if r.IsError || !strings.Contains(text, "must survive the repair") {
					t.Errorf("first recall_memory after the repair = %s, want the pre-damage memory", text)
				}
			} else {
				for _, want := range []string{"could not open its store", "recarto repair"} {
					if !r.IsError || !strings.Contains(text, want) {
						t.Errorf("recall_memory with repair off = %s, want an error mentioning %q", text, want)
					}
				}
			}
			wantServeClosed(t, wait())
		})
	}
}

// serve marks every repo it serves running for the hooks, a RECARTO_REPOS
// extra included, from before the handshake until it ends.
func TestServeMarksEveryServedRepo(t *testing.T) {
	clearRecartoEnv(t)
	cfg := serveCfg(t, t.TempDir(), false)
	extra := t.TempDir()
	initGitRepo(t, extra, "Extra")
	t.Setenv("RECARTO_REPOS", extra)
	hooked, err := app.Open(app.Config{CWD: extra})
	if err != nil {
		t.Fatal(err)
	}
	extraID := hooked.Git.RepoID
	if err := hooked.Close(); err != nil {
		t.Fatal(err)
	}
	sv, err := app.Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{sv.RepoIDs[0], extraID}
	running := func() []bool { return []bool{app.ServerRunning(ids[0]), app.ServerRunning(ids[1])} }
	if got := running(); got[0] || got[1] {
		t.Fatalf("running before serve: %v", got)
	}
	_, wait := startServe(t, cfg, realOpen(cfg))
	if got := running(); !got[0] || !got[1] {
		t.Errorf("running while serving (primary, extra) = %v, want both", got)
	}
	wantServeClosed(t, wait())
	if got := running(); got[0] || got[1] {
		t.Errorf("running after serve ended: %v", got)
	}
}
