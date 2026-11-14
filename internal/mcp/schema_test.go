package mcp

import (
	"testing"

	"github.com/gerwim/recarto/internal/memory"
)

func TestRecordMemorySchemaHasKindEnum(t *testing.T) {
	sc := recordMemorySchema(newTestApp(t, "main"))
	p := sc.Properties["kind"]
	if p == nil {
		t.Fatal("no kind property")
	}
	if len(p.Enum) != len(memory.AllKinds) {
		t.Fatalf("enum len = %d, want %d", len(p.Enum), len(memory.AllKinds))
	}
	got := map[string]bool{}
	for _, v := range p.Enum {
		got[v.(string)] = true
	}
	if !got["todo"] {
		t.Error("kind enum missing todo")
	}
}

func TestTraceAndQueryEnums(t *testing.T) {
	a := newTestApp(t, "main")
	if got := traceCallPathSchema(a).Properties["direction"].Enum; len(got) != 2 {
		t.Errorf("direction enum = %v, want 2 values", got)
	}
	if got := queryGraphSchema(a).Properties["edge_direction"].Enum; len(got) != 2 {
		t.Errorf("edge_direction enum = %v, want 2 values", got)
	}
}
