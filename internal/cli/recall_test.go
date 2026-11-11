package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gerwim/recarto/internal/memory"
)

func TestRecallPrintsCaveat(t *testing.T) {
	a := newTestApp(t, "feat/x")
	ctx := context.Background()
	// A graphed worktree (one present file) so the resolver engages, but the
	// memory references an absent file → it must dangle.
	if _, err := a.Store.DB.ExecContext(ctx,
		`INSERT INTO files(id,worktree,path,language,content_hash,mtime,size,last_indexed_at)
		 VALUES ('p',?,?,?,?,?,?,?)`, a.Git.Worktree, "present.go", "go", "h", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindLearning, Title: "ghost", Body: "ghost learning about absent.go",
		Refs: []memory.Ref{{Type: memory.RefFile, Value: "absent.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Recall(ctx, a, &buf, []string{"ghost", "learning"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not present in this worktree") {
		t.Fatalf("expected caveat in output, got:\n%s", buf.String())
	}
}

func TestRecallByFileRef(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	if _, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind:   memory.KindLearning,
		Title:  "WAL setup",
		Body:   "use wal mode and busy_timeout for cross process writers",
		Refs:   []memory.Ref{{Type: memory.RefFile, Value: "internal/store/store.go"}},
		Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Recall(ctx, a, &buf, []string{"--files", "internal/store/store.go"}); err != nil {
		t.Fatal(err)
	}
	// Assert the full result line, pinning the bullet + kind-bracket format.
	if !strings.Contains(buf.String(), "• [learning] WAL setup") {
		t.Errorf("recall missing/!matching structural result line:\n%s", buf.String())
	}
}

func TestRecallEmpty(t *testing.T) {
	a := newTestApp(t, "main")
	var buf bytes.Buffer
	if err := Recall(context.Background(), a, &buf, []string{"--files", "nope/missing.go"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no memories recalled") {
		t.Errorf("expected empty-recall notice, got: %q", buf.String())
	}
}
