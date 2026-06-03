package claudecode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
