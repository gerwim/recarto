package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// resultText concatenates text content and any structured content as JSON, so the
// assertion is robust to how the SDK serializes a typed tool result.
func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.StructuredContent != nil {
		j, _ := json.Marshal(res.StructuredContent)
		b.Write(j)
	}
	return b.String()
}

func TestServerRecordRecallRoundTrip(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()

	srv := NewServer(a)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Wait() }()

	rec, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name: "record_memory",
		Arguments: map[string]any{
			"kind":  "learning",
			"body":  "use wal mode and busy_timeout for cross process writers",
			"files": []any{"internal/store/store.go"},
		},
	})
	if err != nil {
		t.Fatalf("record_memory call: %v", err)
	}
	if rec.IsError {
		t.Fatalf("record_memory tool error: %s", resultText(rec))
	}

	rc, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "recall_memory",
		Arguments: map[string]any{"files": []any{"internal/store/store.go"}},
	})
	if err != nil {
		t.Fatalf("recall_memory call: %v", err)
	}
	if rc.IsError {
		t.Fatalf("recall_memory tool error: %s", resultText(rc))
	}
	if !strings.Contains(resultText(rc), "wal mode") {
		t.Errorf("recall did not surface the recorded memory:\n%s", resultText(rc))
	}
}

func TestServerInvalidKindIsRejected(t *testing.T) {
	a := newTestApp(t, "main")
	ctx := context.Background()
	srv := NewServer(a)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); _ = ss.Wait() }()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "record_memory",
		Arguments: map[string]any{"kind": "bogus", "body": "x"},
	})
	// The SDK surfaces a handler error either as a transport error or as an
	// IsError result; accept either as "the bad kind was rejected".
	if err == nil && (res == nil || !res.IsError) {
		t.Errorf("expected invalid kind to be rejected, got res=%+v err=%v", res, err)
	}
}
