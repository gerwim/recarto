package memory

import (
	"context"
	"testing"
	"time"

	"git.gerwim.nl/gerwim/recarto/internal/clock"
)

func TestCompleteTodoTransitionsActiveToDone(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	id, _, _, err := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "wire the thing", Body: "wire the thing up", Branch: "main", Source: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := e.CompleteTodo(ctx, id)
	if err != nil || !ok {
		t.Fatalf("CompleteTodo = (%v, %v), want (true, nil)", ok, err)
	}
	rec, err := e.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "done" {
		t.Errorf("status = %q, want \"done\"", rec.Status)
	}

	ok, err = e.CompleteTodo(ctx, id)
	if err != nil || ok {
		t.Fatalf("second CompleteTodo = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = e.CompleteTodo(ctx, "no-such-id")
	if err != nil || ok {
		t.Fatalf("CompleteTodo(unknown) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestActiveTodosExcludesDone(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	openID, _, _, _ := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "open one", Body: "open one body", Branch: "main", Source: "cli"})
	doneID, _, _, _ := e.Record(ctx, NewMemory{Kind: KindTodo, Title: "done one", Body: "done one body", Branch: "main", Source: "cli"})
	if _, err := e.CompleteTodo(ctx, doneID); err != nil {
		t.Fatal(err)
	}

	got, err := e.ActiveTodos(ctx, "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != openID {
		t.Fatalf("ActiveTodos returned %d rows; want only the open todo %s", len(got), openID)
	}
	_ = doneID
}

func TestTodosForCodeMatchesRef(t *testing.T) {
	clk := clock.NewFake(time.UnixMilli(1_000_000))
	e := newTestEngine(t, clk)
	ctx := context.Background()

	hitID, _, _, _ := e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "refactor handler", Body: "refactor the handler",
		Refs: []Ref{{Type: RefFile, Value: "internal/agents/handle.go"}}, Branch: "main", Source: "cli",
	})
	_, _, _, _ = e.Record(ctx, NewMemory{
		Kind: KindTodo, Title: "other", Body: "other body",
		Refs: []Ref{{Type: RefFile, Value: "internal/store/store.go"}}, Branch: "main", Source: "cli",
	})

	got, err := e.TodosForCode(ctx, "main", []string{"internal/agents/handle.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != hitID {
		t.Fatalf("TodosForCode returned %d rows; want only %s", len(got), hitID)
	}

	if _, err := e.CompleteTodo(ctx, hitID); err != nil {
		t.Fatal(err)
	}
	got, err = e.TodosForCode(ctx, "main", []string{"internal/agents/handle.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("completed todo still surfaced: %d rows", len(got))
	}
}
