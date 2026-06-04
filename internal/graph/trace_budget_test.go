package graph

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTraceUncertaintyIsTerminal(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// x.Frob() has no type binding for x and Frob is defined on TWO classes →
	// ambiguous name-only (low). Each Frob calls a Deeper. Tracing callees of M
	// must stop at the low Frob hops and never reach either Deeper.
	src := []byte("namespace N;\n" +
		"class T1 { public void Frob() { Deeper1(); } void Deeper1() {} }\n" +
		"class T2 { public void Frob() { Deeper2(); } void Deeper2() {} }\n" +
		"class Svc { void M() { x.Frob(); } }")
	if err := e.IndexFile(ctx, "/wt", "Svc.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "N.Svc.M", "callees", 3)
	if err != nil {
		t.Fatal(err)
	}
	sawFrobLow := false
	for _, ch := range chains {
		for _, n := range ch {
			if n.Name == "Deeper1" || n.Name == "Deeper2" {
				t.Errorf("%s reached past a low-confidence Frob hop; chains=%v", n.Name, chains)
			}
			if n.Name == "Frob" && n.Confidence == "low" {
				sawFrobLow = true
			}
		}
	}
	if !sawFrobLow {
		t.Errorf("expected a low-confidence Frob hop; chains=%v", chains)
	}
}

func TestTraceGlobalBudget(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	// Root calls a0..a39 (sibling self-calls, high); each a_i calls a distinct
	// c_i (also a high self-call). Expansion exceeds the node budget, so the trace
	// must truncate with a marker and keep total real nodes bounded.
	var b strings.Builder
	b.WriteString("namespace N;\nclass C {\n  void Root() {")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "a%d();", i)
	}
	b.WriteString("}\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "  void a%d() { c%d(); }\n  void c%d() {}\n", i, i, i)
	}
	b.WriteString("}\n")
	src := []byte(b.String())
	if err := e.IndexFile(ctx, "/wt", "C.cs", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}
	if err := e.Resolve(ctx, "/wt"); err != nil {
		t.Fatal(err)
	}
	chains, err := e.TraceCallPath(ctx, "/wt", "N.C.Root", "callees", 3)
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[string]bool{}
	truncated := false
	for _, ch := range chains {
		for _, n := range ch {
			if n.NodeID != "" {
				distinct[n.NodeID] = true
			}
			if strings.Contains(n.Name, "budget reached") {
				truncated = true
			}
		}
	}
	if !truncated {
		t.Errorf("expected a budget-truncation marker; got %d chains", len(chains))
	}
	if len(distinct) > 41 { // budget 40 + the start node
		t.Errorf("emitted %d distinct nodes, want <= 41 (budget)", len(distinct))
	}
}
