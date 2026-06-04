package graph

import (
	"context"
	"testing"
)

func TestIndexFilePersistsReceiverBindingsAndGoQName(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	src := []byte("package p\n" +
		"type Saga struct{}\n" +
		"func (s *Saga) Handle() { s.step() }\n" +
		"func (s *Saga) step() {}\n")
	if err := e.IndexFile(ctx, "/wt", "saga.go", src, 1, int64(len(src))); err != nil {
		t.Fatal(err)
	}

	// Go method qualified name carries the receiver type.
	var qn string
	if err := e.st.DB.QueryRow(
		`SELECT qualified_name FROM nodes WHERE name='Handle'`).Scan(&qn); err != nil {
		t.Fatal(err)
	}
	if qn != "Saga.Handle" {
		t.Errorf("Handle qualified_name = %q, want Saga.Handle", qn)
	}

	// The call edge stored its receiver.
	var recv string
	if err := e.st.DB.QueryRow(
		`SELECT dst_recv FROM edges WHERE kind='calls' AND dst_name='step'`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	if recv != "s" {
		t.Errorf("step call dst_recv = %q, want s", recv)
	}

	// The receiver var binding is scoped to the Handle method body.
	var typeName string
	var scopeStart, scopeEnd int
	if err := e.st.DB.QueryRow(
		`SELECT type_name, scope_start_byte, scope_end_byte FROM type_bindings WHERE name='s' AND type_name='Saga' LIMIT 1`).
		Scan(&typeName, &scopeStart, &scopeEnd); err != nil {
		t.Fatal(err)
	}
	if scopeEnd <= scopeStart {
		t.Errorf("binding scope invalid: [%d,%d]", scopeStart, scopeEnd)
	}
}
