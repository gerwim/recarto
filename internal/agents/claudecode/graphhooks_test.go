package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestPostEditIndexesFile(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Edited(){}"), 0o644)
	payload := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	if err := PostEdit(context.Background(), a, strings.NewReader(payload), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var n int
	a.Store.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name='Edited'`).Scan(&n)
	if n != 1 {
		t.Errorf("Edited node count = %d, want 1 (file not indexed)", n)
	}
}

func TestAugmentInjectsSearchContext(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	pe := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	PostEdit(context.Background(), a, strings.NewReader(pe), &bytes.Buffer{})

	var out bytes.Buffer
	aug := `{"session_id":"s1","tool_input":{"pattern":"Authenticate"}}`
	if err := Augment(context.Background(), a, strings.NewReader(aug), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Authenticate") {
		t.Errorf("augment output = %q, want it to mention Authenticate", out.String())
	}
}

// TestAugmentSkipsFileGlobNoise guards the glob-noise fix: a Glob pattern like
// "*.cs" is a path query, not a symbol query. Feeding its literal stem ("cs") to
// symbol search injects irrelevant matches (e.g. a function named cs in a
// minified bundle). The augment hook must inject nothing for a stemless file
// glob, while still injecting for a real identifier search.
func TestAugmentSkipsFileGlobNoise(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc cs(){}\nfunc Processor(){}"), 0o644)
	pe := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	PostEdit(context.Background(), a, strings.NewReader(pe), &bytes.Buffer{})

	var globOut bytes.Buffer
	glob := `{"session_id":"s1","tool_input":{"pattern":"*.cs"}}`
	if err := Augment(context.Background(), a, strings.NewReader(glob), &globOut); err != nil {
		t.Fatal(err)
	}
	if globOut.Len() != 0 {
		t.Errorf("file-glob augment must inject nothing, got: %q", globOut.String())
	}

	// Regression guard: a genuine identifier search still injects its symbol.
	var idOut bytes.Buffer
	id := `{"session_id":"s2","tool_input":{"pattern":"Processor"}}`
	if err := Augment(context.Background(), a, strings.NewReader(id), &idOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idOut.String(), "Processor") {
		t.Errorf("identifier augment should still inject the symbol, got: %q", idOut.String())
	}
}

func TestPostEditSurfacesBugForEditedFile(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc F(){}"), 0o644)
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "edge case in F", Body: "edge case in F — fix: clamp",
		Refs:   []memory.Ref{{Type: memory.RefFile, Value: "a.go"}},
		Source: "cli", Extra: map[string]any{"fix": "clamp"},
	}); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"s1","tool_input":{"file_path":"` + filepath.Join(wt, "a.go") + `"}}`
	var out bytes.Buffer
	if err := PostEdit(ctx, a, strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "edge case in F") {
		t.Errorf("post-edit should surface the prior bug for the edited file:\n%s", out.String())
	}
}

func TestAugmentSurfacesBugsMatchingPattern(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc Authenticate(){}"), 0o644)
	ctx := context.Background()
	PostEdit(ctx, a, strings.NewReader(`{"session_id":"s1","tool_input":{"file_path":"`+filepath.Join(wt, "a.go")+`"}}`), &bytes.Buffer{})
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Authenticate bypass", Body: "Authenticate bypass when token empty — fix: reject",
		Source: "cli", Extra: map[string]any{"fix": "reject"},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Augment(ctx, a, strings.NewReader(`{"session_id":"s1","tool_input":{"pattern":"Authenticate"}}`), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "Authenticate") { // graph symbol match (existing behavior)
		t.Errorf("augment lost its symbol match:\n%s", s)
	}
	if !strings.Contains(s, "Authenticate bypass") { // new: matching bug
		t.Errorf("augment should surface the matching bug:\n%s", s)
	}
}

// A Bash grep reaches the shell hook as Claude Code sends it and injects the
// Grep tool's hint plus the first-search nudge as PostToolUse context.
func TestShellSearchThroughClaudePayload(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "Widget overflow", Body: "Widget overflow", Source: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	in := `{"session_id":"s1","cwd":"/repo","hook_event_name":"PostToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"cd internal && grep -rn \"Widget\" . | head -20","description":"search"},` +
		`"tool_response":{"stdout":"","stderr":"","interrupted":false}}`
	var out bytes.Buffer
	if err := Shell(ctx, a, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var res struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	h := res.HookSpecificOutput
	if h.HookEventName != "PostToolUse" || !strings.Contains(h.AdditionalContext, "Widget overflow") || !strings.Contains(h.AdditionalContext, "search_content") {
		t.Errorf("got %+v", h)
	}
}
