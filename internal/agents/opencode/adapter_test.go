package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"git.gerwim.nl/gerwim/recarto/internal/agents"
)

func TestDecodeNeutral(t *testing.T) {
	in := strings.NewReader(`{"session_id":"s","file_path":"/a.go","pattern":"x"}`)
	ev, err := Adapter{}.Decode(agents.KindPostEdit, in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.FilePath != "/a.go" || ev.Agent != "opencode" {
		t.Errorf("ev = %+v", ev)
	}
}

func TestEncodeNeutral(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindSessionStart, &sb, agents.Result{Context: "ctx"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `"additionalContext"`) || !strings.Contains(sb.String(), "ctx") {
		t.Errorf("out = %s", sb.String())
	}
	if strings.Contains(sb.String(), "decision") {
		t.Errorf("non-block result carries a decision: %s", sb.String())
	}
}

func TestEncodeStopBlock(t *testing.T) {
	var sb strings.Builder
	if err := (Adapter{}).Encode(agents.KindStop, &sb, agents.Result{Block: true, Reason: "record now"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got["decision"] != "block" || got["reason"] != "record now" {
		t.Errorf("out = %s", sb.String())
	}
}

func TestPluginJSContainsHooksAndBinary(t *testing.T) {
	js := pluginJS(`/opt/recarto`)
	for _, want := range []string{
		`export default { id: "recarto", setup, server };`,
		// OpenCode 2
		`ctx.session.hook("context"`,
		`ctx.tool.hook("execute.after"`,
		"session.execution.succeeded",
		// OpenCode 1
		"experimental.chat.system.transform",
		`"file.edited"`,
		`"tool.execute.after"`,
		`["hook", event, "--agent", "opencode"]`,
		`const BIN = "/opt/recarto";`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("plugin missing %q", want)
		}
	}
	if strings.Contains(js, "__BIN__") {
		t.Errorf("binary placeholder not substituted:\n%s", js)
	}
}

func TestJSStringEscapesWindowsPath(t *testing.T) {
	got := jsString(`C:\bin\recarto.exe`)
	if got != `"C:\\bin\\recarto.exe"` {
		t.Errorf("jsString = %s", got)
	}
}
