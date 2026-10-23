package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookSessionStartDispatch(t *testing.T) {
	a := newTestApp(t, "main")
	var out bytes.Buffer
	in := strings.NewReader(`{"session_id":"s1","source":"startup"}`)
	if err := Hook(context.Background(), a, in, &out, []string{"session-start"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("hook session-start produced no context:\n%s", out.String())
	}
}

func TestHookUnknownEventErrors(t *testing.T) {
	a := newTestApp(t, "main")
	err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, []string{"bogus"})
	if err == nil {
		t.Error("expected error for unknown hook event")
	}
}

func TestHookRequiresEvent(t *testing.T) {
	a := newTestApp(t, "main")
	if err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, nil); err == nil {
		t.Error("expected error when no event is given")
	}
}

func TestHookAgentSelectsCopilotPayload(t *testing.T) {
	a := newTestApp(t, "main")
	// Copilot camelCase payload: a bad (claude) parse would not stamp the session.
	in := strings.NewReader(`{"sessionId":"cop-1","source":"new"}`)
	var out bytes.Buffer
	if err := Hook(context.Background(), a, in, &out, []string{"session-start", "--agent", "copilot"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "additionalContext") {
		t.Errorf("copilot session-start should emit additionalContext:\n%s", out.String())
	}
	var agent string
	if err := a.Store.DB.QueryRowContext(context.Background(),
		`SELECT agent FROM sessions WHERE id='cop-1'`).Scan(&agent); err != nil {
		t.Fatalf("copilot session row not created (payload not parsed as camelCase): %v", err)
	}
	if agent != "copilot" {
		t.Errorf("sessions.agent = %q, want copilot", agent)
	}
}

func TestHookUnknownAgentErrors(t *testing.T) {
	a := newTestApp(t, "main")
	err := Hook(context.Background(), a, strings.NewReader("{}"), &bytes.Buffer{}, []string{"session-start", "--agent", "vim"})
	if err == nil {
		t.Error("expected error for unknown agent")
	}
}

// panicReader panics on the first read — standing in for any bug inside a hook.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("boom") }

// TestRunHookFailsOpen: with no store able to open, every hook kind of every
// host writes nothing to stdout (exit 0 is Run returning nil), and a panic
// inside a hook is swallowed the same way.
func TestRunHookFailsOpen(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECARTO_CACHE_DIR", blocker) // a file: no store can be created under it
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_REPOS", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY"} {
		t.Setenv(k, "")
	}
	file, _ := json.Marshal(filepath.Join(cwd, "a.go")) // inside the served (non-git) cwd
	payloads := map[string]string{
		"claude":   `{"session_id":"s","tool_input":{"file_path":` + string(file) + `,"pattern":"Foo","command":"rg Foo"}}`,
		"copilot":  `{"sessionId":"s","toolArgs":{"path":` + string(file) + `,"pattern":"Foo"}}`,
		"opencode": `{"session_id":"s","file_path":` + string(file) + `,"pattern":"Foo","command":"rg Foo"}`,
	}
	for agent, payload := range payloads {
		for _, event := range []string{"session-start", "post-read", "post-edit", "augment", "stop", "shell"} {
			in := payload
			if agent == "copilot" && event == "shell" {
				in = `{"sessionId":"s","toolName":"bash","toolArgs":{"command":"rg Foo"}}`
			}
			var out bytes.Buffer
			runHook(context.Background(), strings.NewReader(in), &out, []string{event, "--agent", agent})
			if out.Len() != 0 {
				t.Errorf("%s %s with no store: wrote %q, want nothing", agent, event, out.String())
			}
		}
	}
	var out bytes.Buffer
	runHook(context.Background(), panicReader{}, &out, []string{"post-read"})
	if out.Len() != 0 {
		t.Errorf("panicking hook wrote %q", out.String())
	}
}

func TestRunHookPostEditOutsideServedReposOpensNoStore(t *testing.T) {
	t.Chdir(t.TempDir()) // a non-git cwd serves only itself
	cache := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cache)
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_REPOS", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY"} {
		t.Setenv(k, "")
	}
	file, _ := json.Marshal(filepath.Join(t.TempDir(), "x.go"))
	var out bytes.Buffer
	runHook(context.Background(), strings.NewReader(`{"session_id":"s","tool_input":{"file_path":`+string(file)+`}}`), &out, []string{"post-edit"})
	if out.Len() != 0 {
		t.Errorf("output %q", out.String())
	}
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.Name() != "gitctx" {
			t.Errorf("post-edit outside every served repo opened a store: %s", e.Name())
		}
	}
}

// A shell call that neither searches nor prints a served repo's file — or
// whose search another per-command hook answers — opens no store.
func TestRunHookShellWithNothingToDoOpensNoStore(t *testing.T) {
	t.Chdir(t.TempDir()) // a non-git cwd serves only itself
	cwd, _ := os.Getwd()
	cache := t.TempDir()
	t.Setenv("RECARTO_CACHE_DIR", cache)
	for _, k := range []string{"RECARTO_WORKSPACE", "RECARTO_REPOS", "RECARTO_SYNC_URL", "RECARTO_SYNC_KEY"} {
		t.Setenv(k, "")
	}
	outside := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(outside, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ command, cmd string }{
		{"go build ./... && git status", ""},
		{"go test ./... | grep FAIL", "grep"},
		{"cat " + filepath.ToSlash(outside), "cat"},
		{"rg Foo | head", "head"}, // the rg hook answers it
		{"rg Foo | head", "grep"}, // a hook Claude Code would not even start
	} {
		payload, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]string{"command": tc.command}})
		args := []string{"shell"}
		if tc.cmd != "" {
			args = append(args, "--cmd", tc.cmd)
		}
		var out bytes.Buffer
		runHook(context.Background(), bytes.NewReader(payload), &out, args)
		if out.Len() != 0 {
			t.Errorf("%s --cmd %s: output %q", tc.command, tc.cmd, out.String())
		}
	}
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if e.Name() != "gitctx" {
			t.Errorf("a shell call with nothing to do opened a store: %s", e.Name())
		}
	}
}

// The hook whose --cmd owns the line answers it.
func TestHookShellOwnerAnswers(t *testing.T) {
	a := newTestApp(t, "main")
	in := `{"session_id":"s","cwd":"/repo","tool_input":{"command":"rg Foo | head"}}`
	for cmd, want := range map[string]bool{"rg": true, "head": false} {
		var out bytes.Buffer
		if err := Hook(context.Background(), a, strings.NewReader(in), &out, []string{"shell", "--cmd", cmd}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), "search_content"); got != want {
			t.Errorf("--cmd %s answered = %v, want %v: %q", cmd, got, want, out.String())
		}
	}
}
