package claudecode

import (
	"bytes"
	"context"
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

func TestPostEditSurfacesBugForEditedFile(t *testing.T) {
	a := newTestApp(t, "main")
	wt := t.TempDir()
	a.Git.Worktree = wt
	os.WriteFile(filepath.Join(wt, "a.go"), []byte("package p\nfunc F(){}"), 0o644)
	ctx := context.Background()
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindBug, Title: "edge case in F", Body: "edge case in F — fix: clamp",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "a.go"}},
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
	if _, _, err := a.Engine.Record(ctx, memory.NewMemory{
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
