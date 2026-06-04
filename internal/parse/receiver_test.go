package parse

import "testing"

func TestParseCsharpCapturesReceiverAndBindings(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	src := []byte("namespace N;\n" +
		"class A {\n" +
		"  private readonly Dep _dep;\n" +
		"  public Repo Repository { get; set; }\n" +
		"  void M(Other o) { var x = new Thing(); _dep.Run(); }\n" +
		"}")
	res, err := reg.ParseFile("csharp", "A.cs", src)
	if err != nil {
		t.Fatal(err)
	}

	wantBind := map[string]string{"_dep": "Dep", "Repository": "Repo", "o": "Other", "x": "Thing"}
	got := map[string]string{}
	for _, b := range res.Bindings {
		// var x = new Thing() yields both x:var (implicit_type) and x:Thing; keep the real type.
		if got[b.Name] == "" || b.TypeName != "var" {
			got[b.Name] = b.TypeName
		}
	}
	for n, ty := range wantBind {
		if got[n] != ty {
			t.Errorf("binding %s = %q, want %q; all=%v", n, got[n], ty, res.Bindings)
		}
	}
	foundCall := false
	for _, e := range res.Edges {
		if e.Kind == "calls" && e.DstName == "Run" && e.Recv == "_dep" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Errorf("no calls edge Run recv=_dep; edges=%v", res.Edges)
	}
}

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
