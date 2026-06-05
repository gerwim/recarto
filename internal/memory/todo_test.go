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

	id, _, err := e.Record(ctx, NewMemory{
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
