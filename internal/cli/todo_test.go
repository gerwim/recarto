package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/memory"
)

func TestTodoListAndDone(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	id, _, _, err := a.Engine.Record(ctx, memory.NewMemory{
		Kind: memory.KindTodo, Title: "write docs", Body: "write the docs", Branch: a.Git.Branch, Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	var list bytes.Buffer
	if err := Todo(ctx, a, &list, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), "write docs") || !strings.Contains(list.String(), id) {
		t.Fatalf("todo list missing the open todo:\n%s", list.String())
	}

	var done bytes.Buffer
	if err := Todo(ctx, a, &done, []string{"done", id}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(done.String(), "done") {
		t.Fatalf("todo done output unexpected:\n%s", done.String())
	}

	var list2 bytes.Buffer
	if err := Todo(ctx, a, &list2, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list2.String(), id) {
		t.Fatalf("completed todo still listed:\n%s", list2.String())
	}
}
