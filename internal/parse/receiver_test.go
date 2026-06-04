package parse

import "testing"

func TestParseGoCapturesReceiverAndBindings(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	res, err := reg.ParseFile("go", "saga.go", src)
	if err != nil {
		t.Fatal(err)
	}

	var handle *Symbol
	for i := range res.Symbols {
		if res.Symbols[i].Name == "Handle" {
			handle = &res.Symbols[i]
		}
	}
	if handle == nil || handle.RecvType != "Saga" {
		t.Fatalf("Handle RecvType = %v, want Saga", handle)
	}
	foundCall := false
	for _, e := range res.Edges {
		if e.Kind == "calls" && e.DstName == "step" && e.Recv == "s" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Errorf("no calls edge with DstName=step Recv=s; edges=%v", res.Edges)
	}
	foundBind := false
	for _, b := range res.Bindings {
		if b.Name == "s" && b.TypeName == "Saga" {
			foundBind = true
		}
	}
	if !foundBind {
		t.Errorf("no binding s:Saga; bindings=%v", res.Bindings)
	}
}
