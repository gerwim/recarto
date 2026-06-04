package opencode

import (
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
}

func TestPluginJSContainsHooksAndBinary(t *testing.T) {
	js := pluginJS(`/opt/recarto`)
	for _, want := range []string{
		"experimental.chat.system.transform",
		"file.edited",
		"tool.execute.after",
		`run("session-start"`,
		"--agent opencode",
		`const BIN = "/opt/recarto";`,
		"`", // backtick substituted in
	} {
		if !strings.Contains(js, want) {
			t.Errorf("plugin missing %q", want)
		}
	}
	if strings.Contains(js, "~") {
		t.Errorf("backtick placeholder not substituted:\n%s", js)
	}
}

func TestJSStringEscapesWindowsPath(t *testing.T) {
	got := jsString(`C:\bin\recarto.exe`)
	if got != `"C:\\bin\\recarto.exe"` {
		t.Errorf("jsString = %s", got)
	}
}
