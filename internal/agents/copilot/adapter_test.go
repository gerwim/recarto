package copilot

import (
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestDecodePostToolUseRoutesAndExtractsPath(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"edit_file","toolArgs":{"path":"/a/b.go"}}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a/b.go" || ev.Agent != "copilot" {
		t.Errorf("ev = %+v", ev)
	}
}

func TestDecodeAugmentPattern(t *testing.T) {
	in := strings.NewReader(`{"sessionId":"s","toolName":"grep","toolArgs":{"pattern":"foo"}}`)
	ev, err := Adapter{}.Decode(agents.KindAugment, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Pattern != "foo" {
		t.Errorf("pattern = %q", ev.Pattern)
	}
}

func TestEncodeAdditionalContext(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindSessionStart, &sb, agents.Result{Context: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `"additionalContext"`) || !strings.Contains(sb.String(), "hi") {
		t.Errorf("out = %s", sb.String())
	}
}

func TestEncodeStopIsEmptyObject(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindStop, &sb, agents.Result{SystemMessage: "x"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(sb.String()) != "{}" {
		t.Errorf("stop should emit empty object, got %s", sb.String())
	}
}
